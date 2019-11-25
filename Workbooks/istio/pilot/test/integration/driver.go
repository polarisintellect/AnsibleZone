// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/golang/glog"
	// TODO(nmittler): Remove this
	_ "github.com/golang/glog"
	"github.com/golang/sync/errgroup"
	multierror "github.com/hashicorp/go-multierror"
	"k8s.io/client-go/kubernetes"

	meshconfig "istio.io/api/mesh/v1alpha1"
	"istio.io/istio/pilot/platform"
	"istio.io/istio/pilot/platform/kube"
	"istio.io/istio/pilot/platform/kube/inject"
	"istio.io/istio/pilot/test/util"
	"istio.io/istio/pkg/log"
)

var (
	params infra

	// Enable/disable auth, or run both for the tests.
	authmode string
	verbose  bool
	count    int

	// The particular test to run, e.g. "HTTP reachability" or "routing rules"
	testType string

	kubeconfig string
	client     kubernetes.Interface
)

const (
	// retry budget
	budget = 90

	mixerConfigFile     = "/etc/istio/proxy/envoy_mixer.json"
	mixerConfigAuthFile = "/etc/istio/proxy/envoy_mixer_auth.json"

	pilotConfigFile     = "/etc/istio/proxy/envoy_pilot.json"
	pilotConfigAuthFile = "/etc/istio/proxy/envoy_pilot_auth.json"
)

func init() {
	flag.StringVar(&params.Hub, "hub", "gcr.io/istio-testing", "Docker hub")
	flag.StringVar(&params.Tag, "tag", "", "Docker tag")
	flag.StringVar(&params.IstioNamespace, "ns", "",
		"Namespace in which to install Istio components (empty to create/delete temporary one)")
	flag.StringVar(&params.Namespace, "n", "",
		"Namespace in which to install the applications (empty to create/delete temporary one)")
	flag.StringVar(&params.Registry, "registry", string(platform.KubernetesRegistry), "Pilot registry")
	flag.BoolVar(&verbose, "verbose", false, "Debug level noise from proxies")
	flag.BoolVar(&params.checkLogs, "logs", true, "Validate pod logs (expensive in long-running tests)")

	flag.StringVar(&kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"),
		"kube config file (missing or empty file makes the test use in-cluster kube config instead)")
	flag.IntVar(&count, "count", 1, "Number of times to run the tests after deploying")
	flag.StringVar(&authmode, "auth", "both", "Enable / disable auth, or test both.")
	flag.BoolVar(&params.Mixer, "mixer", true, "Enable / disable mixer.")
	flag.StringVar(&params.errorLogsDir, "errorlogsdir", "", "Store per pod logs as individual files in specific directory instead of writing to stderr.")

	// If specified, only run one test
	flag.StringVar(&testType, "testtype", "", "Select test to run (default is all tests)")

	// Keep disabled until default no-op initializer is distributed
	// and running in test clusters.
	flag.BoolVar(&params.UseInitializer, "use-initializer", false, "Use k8s sidecar initializer")
	flag.BoolVar(&params.UseAdmissionWebhook, "use-admission-webhook", false,
		"Use k8s external admission webhook for config validation")

	flag.StringVar(&params.AdmissionServiceName, "admission-service-name", "istio-pilot",
		"Name of admission webhook service name")

	flag.IntVar(&params.DebugPort, "debugport", 0, "Debugging port")

	flag.BoolVar(&params.debugImagesAndMode, "debug", true, "Use debug images and mode (false for prod)")
	flag.BoolVar(&params.SkipCleanup, "skip-cleanup", false, "Debug, skip clean up")
	flag.BoolVar(&params.SkipCleanupOnFailure, "skip-cleanup-on-failure", false, "Debug, skip clean up on failure")
}

type test interface {
	String() string
	setup() error
	run() error
	teardown()
}

func main() {
	flag.Parse()
	_ = log.Configure(log.NewOptions())

	if params.Tag == "" {
		log.Error("No docker tag specified")
		os.Exit(-1)
	}

	if verbose {
		params.Verbosity = 3
	} else {
		params.Verbosity = 2
	}

	params.Name = "(default infra)"
	params.Auth = meshconfig.MeshConfig_NONE
	params.Ingress = true
	params.Zipkin = true
	params.MixerCustomConfigFile = mixerConfigFile
	params.PilotCustomConfigFile = pilotConfigFile

	if len(params.Namespace) != 0 && authmode == "both" {
		log.Infof("When namespace(=%s) is specified, auth mode(=%s) must be one of enable or disable.",
			params.Namespace, authmode)
		return
	}

	if len(kubeconfig) == 0 {
		kubeconfig = "pilot/platform/kube/config"
		glog.Info("Using linked in kube config. Set KUBECONFIG env before running the test.")
	}
	var err error
	_, client, err = kube.CreateInterface(kubeconfig)
	if err != nil {
		log.Errora(err)
		os.Exit(-1)
	}

	switch authmode {
	case "enable":
		runTests(setAuth(params))
	case "disable":
		runTests(params)
	case "both":
		runTests(params, setAuth(params))
	default:
		log.Infof("Invald auth flag: %s. Please choose from: enable/disable/both.", authmode)
	}
}

func setAuth(params infra) infra {
	out := params
	out.Name = "(auth infra)"
	out.Auth = meshconfig.MeshConfig_MUTUAL_TLS
	out.ControlPlaneAuthPolicy = meshconfig.AuthenticationPolicy_MUTUAL_TLS
	out.MixerCustomConfigFile = mixerConfigAuthFile
	out.PilotCustomConfigFile = pilotConfigAuthFile
	return out
}

func tlog(header, s string) {
	log.Infof("\n\n=================== %s =====================\n%s\n\n", header, s)
}

func tlogError(header, s string) {
	log.Errorf("\n\n=================== %s =====================\n%s\n\n", header, s)
}

func tlogFatal(header, s string) {
	tlogError(header, s)
	os.Exit(-1)
}

func runTests(envs ...infra) {
	var result error
	for _, istio := range envs {
		var errs error
		tlog("Deploying infrastructure", spew.Sdump(istio))
		if err := istio.setup(); err != nil {
			result = multierror.Append(result, err)
			continue
		}
		if err := istio.deployApps(); err != nil {
			result = multierror.Append(result, err)
			continue
		}

		nslist := []string{istio.IstioNamespace, istio.Namespace}
		istio.apps, errs = util.GetAppPods(client, kubeconfig, nslist)
		if errs != nil {
			result = multierror.Append(result, errs)
			break
		}

		tests := []test{
			&http{infra: &istio},
			&grpc{infra: &istio},
			&tcp{infra: &istio},
			&headless{infra: &istio},
			&ingress{infra: &istio},
			&egressRules{infra: &istio},
			&routing{infra: &istio},
			&routingToEgress{infra: &istio},
			&zipkin{infra: &istio},
			&authExclusion{infra: &istio},
		}

		for _, test := range tests {
			// If the user has specified a test, skip all other tests
			if len(testType) > 0 && testType != test.String() {
				continue
			}

			for i := 0; i < count; i++ {
				tlog("Test run", strconv.Itoa(i))
				if err := test.setup(); err != nil {
					errs = multierror.Append(errs, multierror.Prefix(err, test.String()))
				} else {
					tlog("Running test", test.String())
					if err := test.run(); err != nil {
						errs = multierror.Append(errs, multierror.Prefix(err, fmt.Sprintf("%v run %d", test, i)))
						tlog("Failed", test.String()+" "+err.Error())
					} else {
						tlog("Success!", test.String())
					}
				}
				tlog("Tearing down test", test.String())
				test.teardown()
			}
		}

		// spill all logs on error
		if errs != nil {
			for _, pod := range util.GetPods(client, istio.Namespace) {
				var filename, content string
				if strings.HasPrefix(pod, "istio-pilot") {
					tlog("Discovery log", pod)
					filename = "istio-pilot"
					content = util.FetchLogs(client, pod, istio.IstioNamespace, "discovery")
				} else if strings.HasPrefix(pod, "istio-mixer") {
					tlog("Mixer log", pod)
					filename = "istio-mixer"
					content = util.FetchLogs(client, pod, istio.IstioNamespace, "mixer")
				} else if strings.HasPrefix(pod, "istio-ingress") {
					tlog("Ingress log", pod)
					filename = "istio-ingress"
					content = util.FetchLogs(client, pod, istio.IstioNamespace, inject.ProxyContainerName)
				} else {
					tlog("Proxy log", pod)
					filename = pod
					content = util.FetchLogs(client, pod, istio.Namespace, inject.ProxyContainerName)
				}

				if len(istio.errorLogsDir) > 0 {
					if err := ioutil.WriteFile(istio.errorLogsDir+"/"+filename+".txt", []byte(content), 0644); err != nil {
						log.Errorf("Failed to save logs to %s:%s. Dumping on stderr\n", filename, err)
						log.Info(content)
					}
				} else {
					log.Info(content)
				}
			}
		}

		cleanup := !istio.SkipCleanup

		if errs == nil {
			tlog("Passed all tests!", fmt.Sprintf("tests: %v, count: %d", tests, count))
		} else {
			tlogError("Failed tests!", errs.Error())
			result = multierror.Append(result, multierror.Prefix(errs, istio.Name))
			if istio.SkipCleanupOnFailure {
				cleanup = false
			}
		}
		if cleanup {
			tlog("Tearing down infrastructure", istio.Name)
			istio.teardown()
		} else {
			tlog("Skipping teardown", istio.Name)
		}
	}

	if result == nil {
		tlog("Passed infrastructure tests!", spew.Sdump(envs))
	} else {
		tlogFatal("Failed infrastructure tests!", result.Error())
	}
}

// fill a file based on a template
func fill(inFile string, values interface{}) (string, error) {
	var bytes bytes.Buffer
	w := bufio.NewWriter(&bytes)

	tmpl, err := template.ParseFiles("pilot/test/integration/testdata/" + inFile)
	if err != nil {
		return "", err
	}

	if err := tmpl.Execute(w, values); err != nil {
		return "", err
	}

	if err := w.Flush(); err != nil {
		return "", err
	}

	return bytes.String(), nil
}

type status error

var (
	errAgain status = errors.New("try again")
)

// run in parallel with retries. all funcs must succeed for the function to succeed
func parallel(fs map[string]func() status) error {
	g, ctx := errgroup.WithContext(context.Background())
	repeat := func(name string, f func() status) func() error {
		return func() error {
			for n := 0; n < budget; n++ {
				log.Infof("%s (attempt %d)", name, n)
				err := f()
				switch err {
				case nil:
					// success
					return nil
				case errAgain:
					// do nothing
				default:
					return fmt.Errorf("failed %s at attempt %d: %v", name, n, err)
				}
				select {
				case <-time.After(time.Second):
					// try again
				case <-ctx.Done():
					return nil
				}
			}
			return fmt.Errorf("failed all %d attempts for %s", budget, name)
		}
	}
	for name, f := range fs {
		g.Go(repeat(name, f))
	}
	return g.Wait()
}

// repeat a check up to budget until it does not return an error
func repeat(f func() error, budget int, delay time.Duration) error {
	var errs error
	for i := 0; i < budget; i++ {
		err := f()
		if err == nil {
			return nil
		}
		errs = multierror.Append(errs, multierror.Prefix(err, fmt.Sprintf("attempt %d", i)))
		log.Infof("attempt #%d failed with %v", i, err)
		time.Sleep(delay)
	}
	return errs
}
