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
	"fmt"
	"io/ioutil"
	"os"
	"time"
	// TODO(nmittler): Remove this
	_ "github.com/golang/glog"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"istio.io/istio/pkg/log"
	"istio.io/istio/pkg/version"
	"istio.io/istio/security/pkg/cmd"
	"istio.io/istio/security/pkg/pki/ca"
	"istio.io/istio/security/pkg/pki/ca/controller"
	"istio.io/istio/security/pkg/registry"
	"istio.io/istio/security/pkg/registry/kube"
	"istio.io/istio/security/pkg/server/grpc"
)

const (
	defaultCACertTTL = 365 * 24 * time.Hour

	defaultWorkloadCertTTL = time.Hour

	maxWorkloadCertTTL = 7 * 24 * time.Hour

	// The default issuer organization for self-signed CA certificate.
	selfSignedCAOrgDefault = "k8s.cluster.local"

	// The key for the environment variable that specifies the namespace.
	namespaceKey = "NAMESPACE"
)

type cliOptions struct {
	certChainFile   string
	signingCertFile string
	signingKeyFile  string
	rootCertFile    string

	namespace string

	istioCaStorageNamespace string

	kubeConfigFile string

	selfSignedCA    bool
	selfSignedCAOrg string

	caCertTTL          time.Duration
	workloadCertTTL    time.Duration
	maxWorkloadCertTTL time.Duration

	grpcHostname string
	grpcPort     int

	loggingOptions *log.Options
}

var (
	opts = cliOptions{
		loggingOptions: log.NewOptions(),
	}

	rootCmd = &cobra.Command{
		Use:   "istio_ca",
		Short: "Istio Certificate Authority (CA)",
		Run: func(cmd *cobra.Command, args []string) {
			runCA()
		},
	}
)

func fatalf(template string, args ...interface{}) {
	log.Errorf(template, args)
	os.Exit(-1)
}

func init() {
	flags := rootCmd.Flags()

	flags.StringVar(&opts.certChainFile, "cert-chain", "", "Speicifies path to the certificate chain file")
	flags.StringVar(&opts.signingCertFile, "signing-cert", "", "Specifies path to the CA signing certificate file")
	flags.StringVar(&opts.signingKeyFile, "signing-key", "", "Specifies path to the CA signing key file")
	flags.StringVar(&opts.rootCertFile, "root-cert", "", "Specifies path to the root certificate file")

	flags.StringVar(&opts.namespace, "namespace", "",
		"Select a namespace for the CA to listen to. If unspecified, Istio CA tries to use the ${"+namespaceKey+"} "+
			"environment variable. If neither is set, Istio CA listens to all namespaces.")
	flags.StringVar(&opts.istioCaStorageNamespace, "istio-ca-storage-namespace", "istio-system", "Namespace where "+
		"the Istio CA pods is running. Will not be used if explicit file or other storage mechanism is specified.")

	flags.StringVar(&opts.kubeConfigFile, "kube-config", "",
		"Specifies path to kubeconfig file. This must be specified when not running inside a Kubernetes pod.")

	flags.BoolVar(&opts.selfSignedCA, "self-signed-ca", false,
		"Indicates whether to use auto-generated self-signed CA certificate. "+
			"When set to true, the '--signing-cert' and '--signing-key' options are ignored.")
	flags.StringVar(&opts.selfSignedCAOrg, "self-signed-ca-org", "k8s.cluster.local",
		fmt.Sprintf("The issuer organization used in self-signed CA certificate (default to %s)",
			selfSignedCAOrgDefault))

	flags.DurationVar(&opts.caCertTTL, "ca-cert-ttl", defaultCACertTTL,
		"The TTL of self-signed CA root certificate")
	flags.DurationVar(&opts.workloadCertTTL, "workload-cert-ttl", defaultWorkloadCertTTL, "The TTL of issued workload certificates")
	flags.DurationVar(&opts.maxWorkloadCertTTL, "max-workload-cert-ttl", maxWorkloadCertTTL, "The max TTL of issued workload certificates")

	flags.StringVar(&opts.grpcHostname, "grpc-hostname", "localhost", "Specifies the hostname for GRPC server.")
	flags.IntVar(&opts.grpcPort, "grpc-port", 0, "Specifies the port number for GRPC server. "+
		"If unspecified, Istio CA will not server GRPC request.")

	rootCmd.AddCommand(version.CobraCommand())

	opts.loggingOptions.AttachCobraFlags(rootCmd)
	cmd.InitializeFlags(rootCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		log.Errora(err)
		os.Exit(-1)
	}
}

func runCA() {
	if err := log.Configure(opts.loggingOptions); err != nil {
		fatalf("Failed to configure logging (%v)", err)
	}

	if value, exists := os.LookupEnv(namespaceKey); exists {
		// When -namespace is not set, try to read the namespace from environment variable.
		if opts.namespace == "" {
			opts.namespace = value
		}
		// Use environment variable for istioCaStorageNamespace if it exists
		opts.istioCaStorageNamespace = value
	}

	verifyCommandLineOptions()

	cs := createClientset()
	ca := createCA(cs.CoreV1())
	// For workloads in K8s, we apply the configured workload cert TTL.
	sc := controller.NewSecretController(ca, opts.workloadCertTTL, cs.CoreV1(), opts.namespace)

	stopCh := make(chan struct{})
	sc.Run(stopCh)

	if opts.grpcPort > 0 {
		// start registry if gRPC server is to be started
		reg := registry.GetIdentityRegistry()
		ch := make(chan struct{})

		// monitor service objects with "alpha.istio.io/kubernetes-serviceaccounts" annotation
		serviceController := kube.NewServiceController(cs.CoreV1(), opts.namespace, reg)
		serviceController.Run(ch)

		// monitor service account objects for istio mesh expansion
		serviceAccountController := kube.NewServiceAccountController(cs.CoreV1(), opts.namespace, reg)
		serviceAccountController.Run(ch)

		// The CA API uses cert with the max workload cert TTL.
		grpcServer := grpc.New(ca, opts.maxWorkloadCertTTL, opts.grpcHostname, opts.grpcPort)
		if err := grpcServer.Run(); err != nil {
			// stop the registry-related controllers
			ch <- struct{}{}

			log.Warnf("Failed to start GRPC server with error: %v", err)
		}
	}

	log.Info("Istio CA has started")
	select {} // wait forever
}

func createClientset() *kubernetes.Clientset {
	c := generateConfig()
	cs, err := kubernetes.NewForConfig(c)
	if err != nil {
		fatalf("Failed to create a clientset (error: %s)", err)
	}
	return cs
}

func createCA(core corev1.SecretsGetter) ca.CertificateAuthority {
	if opts.selfSignedCA {
		log.Info("Use self-signed certificate as the CA certificate")

		// TODO(wattli): Refactor this and combine it with NewIstioCA().
		istioCA, err := ca.NewSelfSignedIstioCA(opts.caCertTTL, opts.workloadCertTTL, opts.maxWorkloadCertTTL, opts.selfSignedCAOrg,
			opts.istioCaStorageNamespace, core)
		if err != nil {
			fatalf("Failed to create a self-signed Istio CA (error: %v)", err)
		}
		return istioCA
	}

	var certChainBytes []byte
	if opts.certChainFile != "" {
		certChainBytes = readFile(opts.certChainFile)
	}
	caOpts := &ca.IstioCAOptions{
		CertChainBytes:   certChainBytes,
		CertTTL:          opts.workloadCertTTL,
		MaxCertTTL:       opts.maxWorkloadCertTTL,
		SigningCertBytes: readFile(opts.signingCertFile),
		SigningKeyBytes:  readFile(opts.signingKeyFile),
		RootCertBytes:    readFile(opts.rootCertFile),
	}

	istioCA, err := ca.NewIstioCA(caOpts)
	if err != nil {
		log.Errorf("Failed to create an Istio CA (error: %v)", err)
	}
	return istioCA
}

func generateConfig() *rest.Config {
	if opts.kubeConfigFile != "" {
		c, err := clientcmd.BuildConfigFromFlags("", opts.kubeConfigFile)
		if err != nil {
			fatalf("Failed to create a config object from file %s, (error %v)", opts.kubeConfigFile, err)
		}
		return c
	}

	// When `kubeConfigFile` is unspecified, use the in-cluster configuration.
	c, err := rest.InClusterConfig()
	if err != nil {
		fatalf("Failed to create a in-cluster config (error: %s)", err)
	}
	return c
}

func readFile(filename string) []byte {
	bs, err := ioutil.ReadFile(filename)
	if err != nil {
		fatalf("Failed to read file %s (error: %v)", filename, err)
	}
	return bs
}

func verifyCommandLineOptions() {
	if opts.selfSignedCA {
		return
	}

	if opts.signingCertFile == "" {
		fatalf(
			"No signing cert has been specified. Either specify a cert file via '-signing-cert' option " +
				"or use '-self-signed-ca'")
	}

	if opts.signingKeyFile == "" {
		fatalf(
			"No signing key has been specified. Either specify a key file via '-signing-key' option " +
				"or use '-self-signed-ca'")
	}

	if opts.rootCertFile == "" {
		fatalf(
			"No root cert has been specified. Either specify a root cert file via '-root-cert' option " +
				"or use '-self-signed-ca'")
	}
}
