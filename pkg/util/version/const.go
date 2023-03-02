package version

// Copyright (c) Microsoft Corporation.
// Licensed under the Apache License 2.0.

var GitCommit = "unknown"

// InstallStream describes stream we are defaulting to for all new clusters
var InstallStream = &Stream{
	Version:  NewVersion(4, 4, 33),
	PullSpec: "quay.io/openshift-release-dev/ocp-release@sha256:a035dddd8a5e5c99484138951ef4aba021799b77eb9046f683a5466c23717738",
}

// Streams describes list of streams we support for upgrades
var (
	Streams = []*Stream{
		InstallStream,
		{
			Version:  NewVersion(4, 3, 38),
			PullSpec: "quay.io/openshift-release-dev/ocp-release@sha256:59cc585be7b4ad069a18f6f1a3309391e172192744ee65fa6e499c8b337edda4",
		},
	}
)

// FluentbitImage contains the location of the Fluentbit container image
func FluentbitImage(acr string) string {
	return acr + ".azurecr.io/fluentbit:1.3.9-1"
}

// MdmImage contains the location of the MDM container image
func MdmImage(acr string) string {
	return acr + ".azurecr.io/genevamdm:master_51"
}

// MdsdImage contains the location of the MDSD container image
func MdsdImage(acr string) string {
	return acr + ".azurecr.io/genevamdsd:master_330"
}
