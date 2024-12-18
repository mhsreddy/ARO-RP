package cluster

// Copyright (c) Microsoft Corporation.
// Licensed under the Apache License 2.0.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/msi-dataplane/pkg/dataplane"
	"github.com/Azure/msi-dataplane/pkg/dataplane/swagger"
	"github.com/Azure/msi-dataplane/pkg/store"

	"github.com/Azure/ARO-RP/pkg/api"
	"github.com/Azure/ARO-RP/pkg/env"
	"github.com/Azure/ARO-RP/pkg/util/azureclient/azuresdk/armmsi"
)

const (
	mockMsiCertValidityDays = 90
)

var (
	errClusterMsiNotPresentInResponse = errors.New("cluster msi not present in msi credentials response")
)

// ensureClusterMsiCertificate leverages the MSI dataplane module to fetch the MSI's
// backing certificate (if needed) and store the certificate in the cluster MSI key
// vault. It does not concern itself with whether an existing certificate is valid
// or not; that can be left to the certificate refresher component.
func (m *manager) ensureClusterMsiCertificate(ctx context.Context) error {
	secretName, err := m.clusterMsiSecretName()
	if err != nil {
		return err
	}

	_, err = m.clusterMsiKeyVaultStore.GetCredentialsObject(ctx, secretName)
	if err == nil {
		return nil
	} else if azcoreErr, ok := err.(*azcore.ResponseError); !ok || azcoreErr.StatusCode != http.StatusNotFound {
		return err
	}

	clusterMsiResourceId, err := m.doc.OpenShiftCluster.ClusterMsiResourceId()
	if err != nil {
		return err
	}

	uaMsiRequest := dataplane.UserAssignedMSIRequest{
		IdentityURL: m.doc.OpenShiftCluster.Identity.IdentityURL,
		ResourceIDs: []string{clusterMsiResourceId.String()},
		TenantID:    m.doc.OpenShiftCluster.Identity.TenantID,
	}

	msiCredObj, err := m.msiDataplane.GetUserAssignedIdentities(ctx, uaMsiRequest)
	if err != nil {
		return err
	}

	now := time.Now()

	var expirationDate time.Time
	if m.env.FeatureIsSet(env.FeatureUseMockMsiRp) {
		expirationDate = now.AddDate(0, 0, mockMsiCertValidityDays)
	} else {
		identity, err := getSingleExplicitIdentity(msiCredObj)
		if err != nil {
			return err
		}
		if identity.NotAfter == nil {
			return errors.New("unable to pull NotAfter from the MSI CredentialsObject")
		}

		// The swagger API spec for the MI RP specifies that NotAfter will be "in the format 2017-03-01T14:11:00Z".
		expirationDate, err = time.Parse(time.RFC3339, *identity.NotAfter)
		if err != nil {
			return err
		}
	}

	secretProperties := store.SecretProperties{
		Enabled:   true,
		Expires:   expirationDate,
		Name:      secretName,
		NotBefore: now,
	}

	return m.clusterMsiKeyVaultStore.SetCredentialsObject(ctx, secretProperties, msiCredObj.CredentialsObject)
}

// initializeClusterMsiClients intializes any Azure clients that use the cluster
// MSI certificate.
func (m *manager) initializeClusterMsiClients(ctx context.Context) error {
	secretName, err := m.clusterMsiSecretName()
	if err != nil {
		return err
	}

	kvSecret, err := m.clusterMsiKeyVaultStore.GetCredentialsObject(ctx, secretName)
	if err != nil {
		return err
	}

	cloud, err := m.env.Environment().CloudNameForMsiDataplane()
	if err != nil {
		return err
	}

	uaIdentities, err := dataplane.NewUserAssignedIdentities(kvSecret.CredentialsObject, cloud)
	if err != nil {
		return err
	}

	msiResourceId, err := m.doc.OpenShiftCluster.ClusterMsiResourceId()
	if err != nil {
		return err
	}

	azureCred, err := uaIdentities.GetCredential(msiResourceId.String())
	if err != nil {
		return err
	}

	// Note that we are assuming that all of the platform MIs are in the same subscription as the ARO resource.
	subId := m.subscriptionDoc.ID
	clientOptions := m.env.Environment().ArmClientOptions()
	clusterMsiFederatedIdentityCredentials, err := armmsi.NewFederatedIdentityCredentialsClient(subId, azureCred, clientOptions)
	if err != nil {
		return err
	}

	userAssignedIdentities, err := armmsi.NewUserAssignedIdentitiesClient(subId, azureCred, clientOptions)
	if err != nil {
		return err
	}

	m.clusterMsiFederatedIdentityCredentials = clusterMsiFederatedIdentityCredentials
	m.userAssignedIdentities = userAssignedIdentities
	return nil
}

// clusterMsiSecretName returns the name to store the cluster MSI certificate under in
// the cluster MSI key vault.
func (m *manager) clusterMsiSecretName() (string, error) {
	clusterMsi, err := m.doc.OpenShiftCluster.ClusterMsiResourceId()
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s-%s", m.doc.ID, clusterMsi.Name), nil
}

func (m *manager) clusterIdentityIDs(ctx context.Context) error {
	if !m.doc.OpenShiftCluster.UsesWorkloadIdentity() {
		return fmt.Errorf("clusterIdentityIDs called for CSP cluster")
	}

	clusterMsiResourceId, err := m.doc.OpenShiftCluster.ClusterMsiResourceId()
	if err != nil {
		return err
	}

	uaMsiRequest := dataplane.UserAssignedMSIRequest{
		IdentityURL: m.doc.OpenShiftCluster.Identity.IdentityURL,
		ResourceIDs: []string{clusterMsiResourceId.String()},
		TenantID:    m.doc.OpenShiftCluster.Identity.TenantID,
	}

	msiCredObj, err := m.msiDataplane.GetUserAssignedIdentities(ctx, uaMsiRequest)
	if err != nil {
		return err
	}

	identity, err := getSingleExplicitIdentity(msiCredObj)
	if err != nil {
		return err
	}
	if identity.ClientID == nil || identity.ObjectID == nil {
		return fmt.Errorf("unable to pull clientID and objectID from the MSI CredentialsObject")
	}

	m.doc, err = m.db.PatchWithLease(ctx, m.doc.Key, func(doc *api.OpenShiftClusterDocument) error {
		// we iterate through the existing identities to find the identity matching
		// the expected resourceID with casefolding, to ensure we preserve the
		// passed-in casing on IDs even if it may be incorrect
		for k, v := range doc.OpenShiftCluster.Identity.UserAssignedIdentities {
			if strings.EqualFold(k, clusterMsiResourceId.String()) {
				v.ClientID = *identity.ClientID
				v.PrincipalID = *identity.ObjectID

				doc.OpenShiftCluster.Identity.UserAssignedIdentities[k] = v
				return nil
			}
		}

		return fmt.Errorf("no entries found matching clusterMsiResourceId")
	})

	return err
}

// We expect the GetUserAssignedIdentities request to only ever be made for one identity
// at a time (the cluster MSI) and thus we expect the response to only contain a single
// identity's details.
func getSingleExplicitIdentity(msiCredObj *dataplane.UserAssignedIdentities) (*swagger.NestedCredentialsObject, error) {
	if msiCredObj.ExplicitIdentities == nil ||
		len(msiCredObj.ExplicitIdentities) == 0 ||
		msiCredObj.ExplicitIdentities[0] == nil {
		return nil, errClusterMsiNotPresentInResponse
	}

	return msiCredObj.ExplicitIdentities[0], nil
}

// fixupClusterMsiTenantID repopulates the cluster MSI's tenant ID in the cluster doc by
// getting it from the subscription doc. Note that we are assuming that the MSI is in the
// same tenant as the cluster.
func (m *manager) fixupClusterMsiTenantID(ctx context.Context) error {
	var err error
	m.doc, err = m.db.PatchWithLease(ctx, m.doc.Key, func(doc *api.OpenShiftClusterDocument) error {
		doc.OpenShiftCluster.Identity.TenantID = m.subscriptionDoc.Subscription.Properties.TenantID
		return nil
	})

	return err
}
