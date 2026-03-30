/*
Copyright 2026 SylphxAI.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"net/http"
	"os"

	"github.com/spf13/pflag"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"
	logsv1 "k8s.io/component-base/logs/api/v1"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	runtimecatalog "sigs.k8s.io/cluster-api/exp/runtime/catalog"
	"sigs.k8s.io/cluster-api/exp/runtime/server"

	"github.com/SylphxAI/talos-fleet-controller/internal/handlers"
	"github.com/SylphxAI/talos-fleet-controller/internal/talos"
)

var (
	// catalog contains all information about RuntimeHooks.
	catalog = runtimecatalog.New()

	// Flags.
	profilerAddress string
	webhookPort     int
	webhookCertDir  string
	logOptions      = logs.NewOptions()
)

func init() {
	// Register all CAPI RuntimeHooks in the catalog.
	_ = runtimehooksv1.AddToCatalog(catalog)
}

// InitFlags initializes command-line flags.
func InitFlags(fs *pflag.FlagSet) {
	logsv1.AddFlags(logOptions, fs)

	fs.StringVar(&profilerAddress, "profiler-address", "",
		"Bind address to expose the pprof profiler (e.g. localhost:6060)")

	fs.IntVar(&webhookPort, "webhook-port", 9443,
		"Port for the RuntimeSDK extension webhook server")

	fs.StringVar(&webhookCertDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs/",
		"Directory containing TLS cert and key for the webhook server")
}

func main() {
	setupLog := ctrl.Log.WithName("setup")

	// Parse flags.
	InitFlags(pflag.CommandLine)
	pflag.CommandLine.SetNormalizeFunc(cliflag.WordSepNormalizeFunc)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	if err := pflag.CommandLine.Set("v", "2"); err != nil {
		setupLog.Error(err, "Failed to set default log level")
		os.Exit(1)
	}
	pflag.Parse()

	if err := logsv1.ValidateAndApply(logOptions, nil); err != nil {
		setupLog.Error(err, "Unable to validate and apply log options")
		os.Exit(1)
	}

	ctrl.SetLogger(klog.Background())

	// Optional pprof profiler.
	if profilerAddress != "" {
		klog.Infof("Profiler listening at %s", profilerAddress)
		go func() {
			klog.Info(http.ListenAndServe(profilerAddress, nil)) //nolint:gosec // profiler is opt-in
		}()
	}

	// Initialize the Talos gRPC client.
	// In-cluster: reads from TALOSCONFIG env var or ServiceAccount-injected secret.
	// Local dev: reads from ~/.talos/config.
	setupCtx := context.Background()
	talosClient, err := talos.NewClient(setupCtx)
	if err != nil {
		setupLog.Error(err, "Failed to create Talos client",
			"hint", "ensure kubernetesTalosAPIAccess is enabled or TALOSCONFIG is set")
		os.Exit(1)
	}
	defer func() { _ = talosClient.Close() }()

	// Create the RuntimeSDK extension webhook server.
	webhookServer, err := server.New(server.Options{
		Catalog: catalog,
		Port:    webhookPort,
		CertDir: webhookCertDir,
	})
	if err != nil {
		setupLog.Error(err, "Failed to create webhook server")
		os.Exit(1)
	}

	// Create K8s client for node IP lookups.
	restConfig := ctrl.GetConfigOrDie()
	k8sClient, err := client.New(restConfig, client.Options{})
	if err != nil {
		setupLog.Error(err, "Failed to create K8s client")
		os.Exit(1)
	}

	// Create extension handlers.
	extHandlers := handlers.NewExtensionHandlers(talosClient, k8sClient)

	// Register in-place update hook handlers.
	if err := webhookServer.AddExtensionHandler(server.ExtensionHandler{
		Hook:        runtimehooksv1.CanUpdateMachine,
		Name:        "can-update-machine",
		HandlerFunc: extHandlers.DoCanUpdateMachine,
	}); err != nil {
		setupLog.Error(err, "Failed to register CanUpdateMachine handler")
		os.Exit(1)
	}

	if err := webhookServer.AddExtensionHandler(server.ExtensionHandler{
		Hook:        runtimehooksv1.CanUpdateMachineSet,
		Name:        "can-update-machine-set",
		HandlerFunc: extHandlers.DoCanUpdateMachineSet,
	}); err != nil {
		setupLog.Error(err, "Failed to register CanUpdateMachineSet handler")
		os.Exit(1)
	}

	if err := webhookServer.AddExtensionHandler(server.ExtensionHandler{
		Hook:        runtimehooksv1.UpdateMachine,
		Name:        "update-machine",
		HandlerFunc: extHandlers.DoUpdateMachine,
	}); err != nil {
		setupLog.Error(err, "Failed to register UpdateMachine handler")
		os.Exit(1)
	}

	// Start the server — blocks until SIGINT/SIGTERM.
	ctx := ctrl.SetupSignalHandler()
	setupLog.Info("Starting Talos In-Place Update Extension server",
		"port", webhookPort,
		"certDir", webhookCertDir,
	)
	if err := webhookServer.Start(ctx); err != nil {
		setupLog.Error(err, "Extension server exited with error")
		os.Exit(1)
	}
}
