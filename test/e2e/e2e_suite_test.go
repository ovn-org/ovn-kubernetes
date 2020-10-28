package e2e_test

import (
	"flag"
	"fmt"
	"os"
	"path"
	"testing"

	"github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/config"
	"github.com/onsi/ginkgo/reporters"
	"github.com/onsi/gomega"
	"k8s.io/klog/v2"
	"k8s.io/kubectl/pkg/generated"
	"k8s.io/kubernetes/test/e2e/framework"
	e2econfig "k8s.io/kubernetes/test/e2e/framework/config"
	"k8s.io/kubernetes/test/e2e/framework/testfiles"
	"k8s.io/kubernetes/test/e2e/framework/viperconfig"
	"k8s.io/kubernetes/test/utils/image"
)

// https://github.com/kubernetes/kubernetes/blob/v1.16.4/test/e2e/e2e_test.go#L62

// handleFlags sets up all flags and parses the command line.
func handleFlags() {
	e2econfig.CopyFlags(e2econfig.Flags, flag.CommandLine)
	framework.RegisterCommonFlags(flag.CommandLine)
	framework.RegisterClusterFlags(flag.CommandLine)
	flag.Parse()
}

var viperConfig = flag.String("viper-config", "", "The name of a viper config file (https://github.com/spf13/viper#what-is-viper). All e2e command line parameters can also be configured in such a file. May contain a path and may or may not contain the file suffix. The default is to look for an optional file with `e2e` as base name. If a file is specified explicitly, it must be present.")

// required due to go1.13 issue: https://github.com/onsi/ginkgo/issues/602
func TestMain(m *testing.M) {
	// Register test flags, then parse flags.
	handleFlags()

	// Now that we know which Viper config (if any) was chosen,
	// parse it and update those options which weren't already set via command line flags
	// (which have higher priority).
	if err := viperconfig.ViperizeFlags(*viperConfig, "e2e", flag.CommandLine); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if framework.TestContext.ListImages {
		for _, v := range image.GetImageConfigs() {
			fmt.Println(v.GetE2EImage())
		}
		os.Exit(0)
	}

	framework.AfterReadingAllFlags(&framework.TestContext)

	// TODO: Deprecating repo-root over time... instead just use gobindata_util.go , see #23987.
	// Right now it is still needed, for example by
	// test/e2e/framework/ingress/ingress_utils.go
	// for providing the optional secret.yaml file and by
	// test/e2e/framework/util.go for cluster/log-dump.
	if framework.TestContext.RepoRoot != "" {
		testfiles.AddFileSource(testfiles.RootFileSource{Root: framework.TestContext.RepoRoot})
	}

	// Enable bindata file lookup as fallback.
	testfiles.AddFileSource(testfiles.BindataFileSource{
		Asset:      generated.Asset,
		AssetNames: generated.AssetNames,
	})
	os.Exit(m.Run())
}

func TestE2e(t *testing.T) {
	// Run tests through the Ginkgo runner with output to console + JUnit for reporting
	var r []ginkgo.Reporter
	if framework.TestContext.ReportDir != "" {
		klog.Infof("Saving reports to %s", framework.TestContext.ReportDir)
		// TODO: we should probably only be trying to create this directory once
		// rather than once-per-Ginkgo-node.
		if err := os.MkdirAll(framework.TestContext.ReportDir, 0755); err != nil {
			klog.Errorf("Failed creating report directory: %v", err)
		} else {
			r = append(r, reporters.NewJUnitReporter(path.Join(framework.TestContext.ReportDir, fmt.Sprintf("junit_%v%02d.xml", framework.TestContext.ReportPrefix, config.GinkgoConfig.ParallelNode))))
		}
	}
	gomega.RegisterFailHandler(framework.Fail)
	ginkgo.RunSpecsWithDefaultAndCustomReporters(t, "E2e Suite", r)
}
