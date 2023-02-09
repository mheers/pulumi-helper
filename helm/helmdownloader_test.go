package helm

import (
	"testing"

	"github.com/pulumi/pulumi-kubernetes/provider/v3/pkg/provider"
	"github.com/stretchr/testify/require"
)

func TestDownloadNifi(t *testing.T) {
	src := HelmChartSrc{
		HelmChartOpts: provider.HelmChartOpts{
			Chart: "oci://ghcr.io/konpyutaika/helm-charts/nifikop",
		},
	}
	err := src.Download()
	require.NoError(t, err)
}

func TestDownloadZookeeper(t *testing.T) {
	src := HelmChartSrc{
		HelmChartOpts: provider.HelmChartOpts{
			Chart: "zookeeper",
			HelmFetchOpts: provider.HelmFetchOpts{
				Repo: "https://charts.bitnami.com/bitnami",
			},
		},
	}
	err := src.Download()
	require.NoError(t, err)
}
