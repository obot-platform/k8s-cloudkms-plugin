// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Binary kmsplugin - entry point into kms-plugin. See go/gke-secrets-encryption-design for details.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/k8s-cloudkms-plugin/plugin"
	v1 "github.com/GoogleCloudPlatform/k8s-cloudkms-plugin/plugin/v1"
	v2 "github.com/GoogleCloudPlatform/k8s-cloudkms-plugin/plugin/v2"
	"github.com/golang/glog"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/cloudkms/v1"
	"google.golang.org/api/option"
)

var (
	healthzPort    = flag.Int("healthz-port", 8081, "Port on which to publish healthz")
	healthzPath    = flag.String("healthz-path", "healthz", "Path at which to publish healthz")
	healthzTimeout = flag.Duration("healthz-timeout", 5*time.Second, "timeout in seconds for communicating with the unix socket")

	metricsPort = flag.Int("metrics-port", 8082, "Port on which to publish metrics")
	metricsPath = flag.String("metrics-path", "metrics", "Path at which to publish metrics")

	keyURI           = flag.String("key-uri", "", "Uri of the key use for crypto operations (ex. projects/my-project/locations/my-location/keyRings/my-key-ring/cryptoKeys/my-key)")
	pathToUnixSocket = flag.String("path-to-unix-socket", "/var/run/kmsplugin/socket.sock", "Full path to Unix socket that is used for communicating with KubeAPI Server, or Linux socket namespace object - must start with @")
	kmsVersion       = flag.String("kms", "v2", "Kubernetes KMS API version. Possible values: v1, v2. Default value is v2.")
	keySuffix        = flag.String("key-suffix", "", "Set to a unique value in case if plugin is reconfigured to use Cloud KMS key version that was already in use before. Applicable only in KMS API v2 mode")

	// Integration testing arguments.
	integrationTest = flag.Bool("integration-test", false, "When set to true, http.DefaultClient will be used, as opposed callers identity acquired with a TokenService.")
	fakeKMSPort     = flag.Int("fake-kms-port", 8085, "Port for Fake KMS, only use in integration tests.")
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	flag.Parse()
	mustValidateFlags()

	var (
		httpClient = http.DefaultClient
		err        error
	)

	if !*integrationTest {
		credentials, err := google.FindDefaultCredentials(ctx, cloudkms.CloudPlatformScope)
		if err != nil {
			glog.Exitf("failed to get default credentials: %v", err)
		}

		t, err := credentials.TokenSource.Token()
		if err != nil {
			glog.Errorf("failed to get token: %v", err)
			os.Exit(1)
		} else if t.AccessToken == "" {
			glog.Errorf("access token is empty")
			os.Exit(1)
		}

		httpClient = oauth2.NewClient(ctx, credentials.TokenSource)
	}

	kms, err := cloudkms.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		glog.Exitf("failed to instantiate cloud kms httpClient: %v", err)
	}

	if *integrationTest {
		kms.BasePath = fmt.Sprintf("http://localhost:%d", *fakeKMSPort)
	}

	metrics := &plugin.Metrics{
		ServingURL: &url.URL{
			Host: fmt.Sprintf("localhost:%d", *metricsPort),
			Path: *metricsPath,
		},
	}

	var p plugin.Plugin
	var healthChecker plugin.HealthChecker
	switch *kmsVersion {
	case "v1":
		p = v1.NewPlugin(kms.Projects.Locations.KeyRings.CryptoKeys, *keyURI)
		healthChecker = v1.NewHealthChecker()
		glog.Info("Kubernetes KMS API v1beta1")
	case "v2":
		p = v2.NewPlugin(kms.Projects.Locations.KeyRings.CryptoKeys, *keyURI, *keySuffix)
		healthChecker = v2.NewHealthChecker()
		glog.Info("Kubernetes KMS API v2")
	default:
		glog.Exitf("invalid value %q for --kms", *kmsVersion)
	}

	hc := plugin.NewHealthChecker(healthChecker, *keyURI, kms.Projects.Locations.KeyRings.CryptoKeys, *pathToUnixSocket, *healthzTimeout, &url.URL{
		Host: fmt.Sprintf("localhost:%d", *healthzPort),
		Path: *healthzPath,
	})

	pluginManager := plugin.NewManager(p, *pathToUnixSocket)

	glog.Exit(run(pluginManager, hc, metrics))
}

func run(pluginManager *plugin.PluginManager, h *plugin.HealthCheckerManager, m *plugin.Metrics) error {
	signalsChan := make(chan os.Signal, 1)
	signal.Notify(signalsChan, syscall.SIGINT, syscall.SIGTERM)

	metricsErrCh := m.Serve()
	healthzErrCh := h.Serve()

	gRPCSrv, kmsErrorCh := pluginManager.Start()
	defer gRPCSrv.GracefulStop()

	for {
		select {
		case sig := <-signalsChan:
			return fmt.Errorf("captured %v, shutting down kms-plugin", sig)
		case kmsError := <-kmsErrorCh:
			return kmsError
		case metricsErr := <-metricsErrCh:
			// Limiting this to warning only - will run without metrics.
			glog.Warning(metricsErr)
			metricsErrCh = nil
		case healthzErr := <-healthzErrCh:
			// Limiting this to warning only - will run without healthz.
			glog.Warning(healthzErr)
			healthzErrCh = nil
		}
	}
}

func mustValidateFlags() {
	if *kmsVersion == "v1" && *keySuffix != "" {
		glog.Exitf("--key-suffix argument cannot be used in v1 mode (--kms=v1)")
	}
	glog.Infof("Checking socket path %q", *pathToUnixSocket)
	socketDir := filepath.Dir(*pathToUnixSocket)
	glog.Infof("Unix Socket directory is %q", socketDir)
	if _, err := os.Stat(socketDir); err != nil {
		glog.Exitf(" Directory %q portion of path-to-unix-socket flag:%q does not seem to exist.", socketDir, *pathToUnixSocket)
	}
	glog.Infof("Communication between KUBE API and KMS Plugin containers will be via %q", *pathToUnixSocket)
}
