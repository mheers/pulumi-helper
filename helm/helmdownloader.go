package helm

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/pulumi/pulumi-kubernetes/provider/v4/pkg/provider"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/registry"
)

const UntarDir = "chart"

type HelmChartSrc struct {
	provider.HelmChartOpts
	DestDir string
}

func (c *HelmChartSrc) Download() error {
	err := c.cleanOldHelmChart()
	if err != nil {
		return err
	}
	return c.fetch()
}

func (c *HelmChartSrc) Path() string {
	return path.Join(c.DestDir, "chart")
}

func (c *HelmChartSrc) cleanOldHelmChart() error {
	p := path.Join(c.DestDir, UntarDir)
	return os.RemoveAll(p)
}

// compare to https://github.com/pulumi/pulumi-kubernetes/blob/master/provider/pkg/provider/invoke_helm_template.go#L134
func (c *HelmChartSrc) fetch() error {
	if c.DestDir == "" {
		c.DestDir = "./"
	}

	registryClient, err := registry.NewClient(
		registry.ClientOptDebug(c.HelmChartDebug),
		registry.ClientOptCredentialsFile(c.HelmRegistryConfig),
	)
	if err != nil {
		return err
	}

	cfg := &action.Configuration{
		RegistryClient: registryClient,
	}
	p := action.NewPullWithOpts(action.WithConfig(cfg))

	p.Settings = cli.New()
	p.CaFile = c.CAFile
	p.CertFile = c.CertFile
	p.DestDir = c.DestDir
	//p.DestDir = c.Destination // currently not used, could be useful for caching some day
	p.KeyFile = c.KeyFile
	p.Keyring = c.Keyring
	p.Password = c.Password
	// c.Prov is unused
	p.RepoURL = c.HelmFetchOpts.Repo
	p.Untar = true
	p.UntarDir = UntarDir
	p.Username = c.Username
	p.Verify = c.Verify

	if len(c.Repo) > 0 && strings.HasPrefix(c.Repo, "http") {
		return errors.New("'repo' option specifies the name of the Helm Chart repo, not the URL." +
			"Use 'fetchOpts.repo' to specify a URL for a remote Chart")
	}

	// TODO: We have two different version parameters, but it doesn't make sense
	// 		 to specify both. We should deprecate the FetchOpts one.

	if len(c.Version) == 0 && len(c.HelmFetchOpts.Version) == 0 {
		if c.Devel {
			p.Version = ">0.0.0-0"
		}
	} else if len(c.Version) > 0 {
		p.Version = c.Version
	} else if len(c.HelmFetchOpts.Version) > 0 {
		p.Version = c.HelmFetchOpts.Version
	} // If both are set, prefer the top-level version over the FetchOpts version.

	chartRef := normalizeChartRef(c.Repo, p.RepoURL, c.Chart)

	downloadInfo, err := p.Run(chartRef)
	if err != nil {
		return errors.New("failed to pull chart")
	}
	fmt.Println(downloadInfo)
	return nil
}

// In case URL is not known we prefix the chart ref with the repoName,
// so for example "apache" becomes "bitnami/apache". We should not
// prefix it when URL is known, as that results in an error such as:
//
// failed to pull chart: chart "bitnami/apache" version "1.0.0" not
// found in https://raw.githubusercontent.com/bitnami/charts/eb5f9a9513d987b519f0ecd732e7031241c50328/bitnami repository
func normalizeChartRef(repoName string, repoURL string, originalChartRef string) string {

	// If URL is known, do not prefix
	if len(repoURL) > 0 || registry.IsOCI(originalChartRef) {
		return originalChartRef
	}

	// Add a prefix if repoName is known and ref is not already prefixed
	prefix := fmt.Sprintf("%s/", strings.TrimSuffix(repoName, "/"))
	if len(repoName) > 0 && !strings.HasPrefix(originalChartRef, prefix) {
		return fmt.Sprintf("%s%s", prefix, originalChartRef)
	}

	// Otherwise leave as-is
	return originalChartRef
}
