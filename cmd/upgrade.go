package cmd

import (
	"fmt"
	"os"
	"strings"

	"errors"

	"github.com/databus23/helm-diff/diff"
	"github.com/databus23/helm-diff/manifest"
	"github.com/spf13/cobra"
	"k8s.io/helm/pkg/helm"
)

type diffCmd struct {
	release          string
	chart            string
	chartVersion     string
	client           helm.Interface
	detailedExitCode bool
	devel            bool
	namespace        string // namespace to assume the release to be installed into. Defaults to the current kube config namespace.
	valueFiles       valueFiles
	values           []string
	stringValues     []string
	reuseValues      bool
	resetValues      bool
	allowUnreleased  bool
	suppressedKinds  []string
	outputContext    int
}

const globalUsage = `Show a diff explaining what a helm upgrade would change.

This fetches the currently deployed version of a release
and compares it to a chart plus values.
This can be used visualize what changes a helm upgrade will
perform.
`

func newChartCommand() *cobra.Command {
	diff := diffCmd{}

	cmd := &cobra.Command{
		Use:     "upgrade [flags] [RELEASE] [CHART]",
		Short:   "Show a diff explaining what a helm upgrade would change.",
		Long:    globalUsage,
		Example: "  helm diff upgrade my-release stable/postgresql --values values.yaml",
		Args: func(cmd *cobra.Command, args []string) error {
			return checkArgsLength(len(args), "release name", "chart path")
		},
		PersistentPreRun: func(*cobra.Command, []string) {
			expandTLSPaths()
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// Suppress the command usage on error. See #77 for more info
			cmd.SilenceUsage = true

			if q, _ := cmd.Flags().GetBool("suppress-secrets"); q {
				diff.suppressedKinds = append(diff.suppressedKinds, "Secret")
			}

			diff.release = args[0]
			diff.chart = args[1]
			if diff.client == nil {
				diff.client = createHelmClient()
			}
			return diff.run()
		},
	}

	f := cmd.Flags()
	f.StringVar(&diff.chartVersion, "version", "", "specify the exact chart version to use. If this is not specified, the latest version is used")
	f.BoolVar(&diff.detailedExitCode, "detailed-exitcode", false, "return a non-zero exit code when there are changes")
	f.BoolP("suppress-secrets", "q", false, "suppress secrets in the output")
	f.VarP(&diff.valueFiles, "values", "f", "specify values in a YAML file (can specify multiple)")
	f.StringArrayVar(&diff.values, "set", []string{}, "set values on the command line (can specify multiple or separate values with commas: key1=val1,key2=val2)")
	f.StringArrayVar(&diff.stringValues, "set-string", []string{}, "set STRING values on the command line (can specify multiple or separate values with commas: key1=val1,key2=val2)")
	f.BoolVar(&diff.reuseValues, "reuse-values", false, "reuse the last release's values and merge in any new values")
	f.BoolVar(&diff.resetValues, "reset-values", false, "reset the values to the ones built into the chart and merge in any new values")
	f.BoolVar(&diff.allowUnreleased, "allow-unreleased", false, "enables diffing of releases that are not yet deployed via Helm")
	f.BoolVar(&diff.devel, "devel", false, "use development versions, too. Equivalent to version '>0.0.0-0'. If --version is set, this is ignored.")
	f.StringArrayVar(&diff.suppressedKinds, "suppress", []string{}, "allows suppression of the values listed in the diff output")
	f.IntVarP(&diff.outputContext, "context", "C", -1, "output NUM lines of context around changes")
	f.StringVar(&diff.namespace, "namespace", "default", "namespace to assume the release to be installed into")

	addCommonCmdOptions(f)

	return cmd

}

func (d *diffCmd) run() error {
	if d.chartVersion == "" && d.devel {
		d.chartVersion = ">0.0.0-0"
	}

	chartPath, err := locateChartPath(d.chart, d.chartVersion, false, "")
	if err != nil {
		return err
	}

	if err := d.valueFiles.Valid(); err != nil {
		return err
	}

	rawVals, err := d.vals()
	if err != nil {
		return err
	}

	releaseResponse, err := d.client.ReleaseContent(d.release)

	var newInstall bool
	if err != nil && strings.Contains(err.Error(), fmt.Sprintf("release: %q not found", d.release)) {
		if d.allowUnreleased {
			fmt.Printf("********************\n\n\tRelease was not present in Helm.  Diff will show entire contents as new.\n\n********************\n")
			newInstall = true
			err = nil
		} else {
			fmt.Printf("********************\n\n\tRelease was not present in Helm.  Include the `--allow-unreleased` to perform diff without exiting in error.\n\n********************\n")
		}

	}

	if err != nil {
		return prettyError(err)
	}

	var currentSpecs, newSpecs map[string]*manifest.MappingResult
	if newInstall {
		installResponse, err := d.client.InstallRelease(
			chartPath,
			d.namespace,
			helm.ReleaseName(d.release),
			helm.ValueOverrides(rawVals),
			helm.InstallDryRun(true),
		)
		if err != nil {
			return prettyError(err)
		}

		currentSpecs = make(map[string]*manifest.MappingResult)
		newSpecs = manifest.Parse(installResponse.Release.Manifest, installResponse.Release.Namespace)
	} else {
		upgradeResponse, err := d.client.UpdateRelease(
			d.release,
			chartPath,
			helm.UpdateValueOverrides(rawVals),
			helm.ReuseValues(d.reuseValues),
			helm.ResetValues(d.resetValues),
			helm.UpgradeDryRun(true),
		)
		if err != nil {
			return prettyError(err)
		}

		currentSpecs = manifest.ParseRelease(releaseResponse.Release)
		newSpecs = manifest.ParseRelease(upgradeResponse.Release)
	}

	seenAnyChanges := diff.DiffManifests(currentSpecs, newSpecs, d.suppressedKinds, d.outputContext, os.Stdout)

	if d.detailedExitCode && seenAnyChanges {
		return Error{
			error: errors.New("identified at least one change, exiting with non-zero exit code (detailed-exitcode parameter enabled)"),
			Code:  2,
		}
	}

	return nil
}
