/*
 * Copyright 2023 The Kubernetes Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v2"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	nodeutil "k8s.io/component-helpers/node/util"
	"k8s.io/klog/v2"

	"github.com/google/dranet/pkg/driver"

	"github.com/MartinForReal/dra-infiniband-driver/internal/ibinventory"
	"github.com/MartinForReal/dra-infiniband-driver/pkg/flags"
)

const (
	defaultDriverName = "ib.sigs.k8s.io"
)

func main() {
	if err := newApp().Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func newApp() *cli.App {
	var (
		kubeconfig       string
		hostnameOverride string
		driverName       string
		numVFs           int
		numSimDevices    int
	)

	loggingConfig := flags.NewLoggingConfig()

	cliFlags := []cli.Flag{
		&cli.StringFlag{
			Name:        "kubeconfig",
			Usage:       "Absolute path to the kubeconfig file.",
			Destination: &kubeconfig,
			EnvVars:     []string{"KUBECONFIG"},
		},
		&cli.StringFlag{
			Name:        "hostname-override",
			Usage:       "If non-empty, will be used as the name of the Node.",
			Destination: &hostnameOverride,
			EnvVars:     []string{"NODE_NAME"},
		},
		&cli.StringFlag{
			Name:        "driver-name",
			Usage:       "Name of the DRA driver.",
			Value:       defaultDriverName,
			Destination: &driverName,
			EnvVars:     []string{"DRIVER_NAME"},
		},
		&cli.IntFlag{
			Name:        "num-vfs",
			Usage:       "Number of SR-IOV VFs to pre-create per PF at startup (0 = no auto-provisioning, i.e., VM mode).",
			Value:       0,
			Destination: &numVFs,
			EnvVars:     []string{"NUM_VFS"},
		},
		&cli.IntFlag{
			Name:        "num-sim-devices",
			Usage:       "Number of simulated IB VFs to create when no real hardware is found (0 = disabled). For testing only.",
			Value:       0,
			Destination: &numSimDevices,
			EnvVars:     []string{"NUM_SIM_DEVICES"},
		},
	}
	cliFlags = append(cliFlags, loggingConfig.Flags()...)

	app := &cli.App{
		Name:            "dra-ib-kubeletplugin",
		Usage:           "DRA InfiniBand driver plugin using the DRANET framework.",
		ArgsUsage:       " ",
		HideHelpCommand: true,
		Flags:           cliFlags,
		Before: func(c *cli.Context) error {
			if c.Args().Len() > 0 {
				return fmt.Errorf("arguments not supported: %v", c.Args().Slice())
			}
			return loggingConfig.Apply()
		},
		Action: func(c *cli.Context) error {
			ctx := c.Context

			// Build Kubernetes client.
			var config *rest.Config
			var err error
			if kubeconfig != "" {
				config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
			} else {
				config, err = rest.InClusterConfig()
			}
			if err != nil {
				return fmt.Errorf("create client-go config: %w", err)
			}

			// Use protobuf for better performance at scale.
			config.AcceptContentTypes = "application/vnd.kubernetes.protobuf,application/json"
			config.ContentType = "application/vnd.kubernetes.protobuf"

			clientset, err := kubernetes.NewForConfig(config)
			if err != nil {
				return fmt.Errorf("create kubernetes clientset: %w", err)
			}

			nodeName, err := nodeutil.GetHostname(hostnameOverride)
			if err != nil {
				return fmt.Errorf("get node name: %w", err)
			}

			// Create the IB inventory adapter that implements DRANET's inventoryDB.
			ibDB := ibinventory.New(
				ibinventory.WithNumVFs(numVFs),
				ibinventory.WithNumSimDevices(numSimDevices),
			)

			// Start the DRANET driver framework.
			// This handles:
			//   - DRA kubelet plugin registration
			//   - NRI plugin for pod sandbox lifecycle hooks
			//   - Resource publishing via ResourceSlice
			//   - PrepareResourceClaims / UnprepareResourceClaims
			//   - Network device namespace management (netdev + RDMA)
			ctx, cancel := context.WithCancel(ctx)

			signalCh := make(chan os.Signal, 1)
			signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)

			dranet, err := driver.Start(ctx, driverName, clientset, nodeName,
				driver.WithInventory(ibDB),
			)
			if err != nil {
				cancel()
				return fmt.Errorf("start DRANET driver: %w", err)
			}
			defer dranet.Stop()

			klog.Infof("IB DRA driver started (driver=%s, node=%s, numVFs=%d, numSimDevices=%d)",
				driverName, nodeName, numVFs, numSimDevices)

			select {
			case sig := <-signalCh:
				klog.Infof("Received shutdown signal: %q", sig)
				cancel()
			case <-ctx.Done():
				klog.Info("Context cancelled, shutting down")
			}

			return nil
		},
	}

	return app
}
