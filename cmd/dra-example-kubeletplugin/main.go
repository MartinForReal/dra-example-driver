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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/urfave/cli/v2"

	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dra-example-driver/internal/profiles"
	"sigs.k8s.io/dra-example-driver/internal/profiles/ib"
	"sigs.k8s.io/dra-example-driver/pkg/flags"
)

const (
	DriverPluginCheckpointFile = "checkpoint.json"
)

type Flags struct {
	kubeClientConfig flags.KubeClientConfig
	loggingConfig    *flags.LoggingConfig

	nodeName                      string
	cdiRoot                       string
	numVFs                        int
	kubeletRegistrarDirectoryPath string
	kubeletPluginsDirectoryPath   string
	healthcheckPort               int
	profile                       string
	driverName                    string
}

type Config struct {
	flags         *Flags
	coreclient    coreclientset.Interface
	cancelMainCtx func(error)

	profile profiles.Profile
}

var validProfiles = map[string]func(flags Flags) profiles.Profile{
	ib.ProfileName: func(flags Flags) profiles.Profile {
		return ib.NewProfile(flags.nodeName, flags.numVFs)
	},
}

var validProfileNames = func() []string {
	var valid []string
	for profileName := range validProfiles {
		valid = append(valid, profileName)
	}
	return valid
}()

func (c Config) DriverPluginPath() string {
	return filepath.Join(c.flags.kubeletPluginsDirectoryPath, c.flags.driverName)
}

func main() {
	if err := newApp().Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func newApp() *cli.App {
	flags := &Flags{
		loggingConfig: flags.NewLoggingConfig(),
	}
	cliFlags := []cli.Flag{
		&cli.StringFlag{
			Name:        "node-name",
			Usage:       "The name of the node to be worked on.",
			Required:    true,
			Destination: &flags.nodeName,
			EnvVars:     []string{"NODE_NAME"},
		},
		&cli.StringFlag{
			Name:        "cdi-root",
			Usage:       "Absolute path to the directory where CDI files will be generated.",
			Value:       "/etc/cdi",
			Destination: &flags.cdiRoot,
			EnvVars:     []string{"CDI_ROOT"},
		},
		&cli.IntFlag{
			Name:        "num-vfs",
			Usage:       "Number of SR-IOV VFs to pre-create per PF at startup (0 = no auto-provisioning, i.e., VM mode).",
			Value:       0,
			Destination: &flags.numVFs,
			EnvVars:     []string{"NUM_VFS"},
		},
		&cli.StringFlag{
			Name:        "kubelet-registrar-directory-path",
			Usage:       "Absolute path to the directory where kubelet stores plugin registrations.",
			Value:       kubeletplugin.KubeletRegistryDir,
			Destination: &flags.kubeletRegistrarDirectoryPath,
			EnvVars:     []string{"KUBELET_REGISTRAR_DIRECTORY_PATH"},
		},
		&cli.StringFlag{
			Name:        "kubelet-plugins-directory-path",
			Usage:       "Absolute path to the directory where kubelet stores plugin data.",
			Value:       kubeletplugin.KubeletPluginsDir,
			Destination: &flags.kubeletPluginsDirectoryPath,
			EnvVars:     []string{"KUBELET_PLUGINS_DIRECTORY_PATH"},
		},
		&cli.IntFlag{
			Name:        "healthcheck-port",
			Usage:       "Port to start a gRPC healthcheck service. When positive, a literal port number. When zero, a random port is allocated. When negative, the healthcheck service is disabled.",
			Value:       -1,
			Destination: &flags.healthcheckPort,
			EnvVars:     []string{"HEALTHCHECK_PORT"},
		},
		&cli.StringFlag{
			Name:        "device-profile",
			Usage:       fmt.Sprintf("Name of the device profile. Valid values are %q.", validProfileNames),
			Value:       ib.ProfileName,
			Destination: &flags.profile,
			EnvVars:     []string{"DEVICE_PROFILE"},
		},
		&cli.StringFlag{
			Name:        "driver-name",
			Usage:       "Name of the DRA driver. Its default is derived from the device profile.",
			Destination: &flags.driverName,
			EnvVars:     []string{"DRIVER_NAME"},
		},
	}
	cliFlags = append(cliFlags, flags.kubeClientConfig.Flags()...)
	cliFlags = append(cliFlags, flags.loggingConfig.Flags()...)

	app := &cli.App{
		Name:            "dra-example-kubeletplugin",
		Usage:           "dra-example-kubeletplugin implements a DRA driver plugin.",
		ArgsUsage:       " ",
		HideHelpCommand: true,
		Flags:           cliFlags,
		Before: func(c *cli.Context) error {
			if c.Args().Len() > 0 && c.Args().First() != "move-netdev" {
				return fmt.Errorf("arguments not supported: %v", c.Args().Slice())
			}
			return flags.loggingConfig.Apply()
		},
		Action: func(c *cli.Context) error {
			ctx := c.Context
			clientSets, err := flags.kubeClientConfig.NewClientSets()
			if err != nil {
				return fmt.Errorf("create client: %w", err)
			}

			if flags.driverName == "" {
				flags.driverName = flags.profile + ".sigs.k8s.io"
			}

			newProfile, ok := validProfiles[flags.profile]
			if !ok {
				return fmt.Errorf("invalid device profile %q, valid profiles are %q", flags.profile, validProfileNames)
			}

			config := &Config{
				flags:      flags,
				coreclient: clientSets.Core,
				profile:    newProfile(*flags),
			}

			return RunPlugin(ctx, config)
		},
		Commands: []*cli.Command{
			moveNetdevCommand(),
		},
	}

	return app
}

// ociState represents the minimal OCI runtime state passed to hooks on stdin.
type ociState struct {
	Pid int `json:"pid"`
}

// moveNetdevCommand returns the CLI subcommand used as a CDI createRuntime hook
// to move the IB netdev (and RDMA device) into the container's network namespace.
func moveNetdevCommand() *cli.Command {
	return &cli.Command{
		Name:      "move-netdev",
		Usage:     "CDI hook helper: move an IB netdev into a container's network namespace",
		Hidden:    true,
		ArgsUsage: " ",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "ib-dev",
				Usage:    "The IB device name (e.g. mlx5_0) whose netdev(s) to move.",
				Required: true,
			},
		},
		Action: func(c *cli.Context) error {
			ctx := c.Context
			logger := klog.FromContext(ctx)
			ibDevName := c.String("ib-dev")

			// OCI runtimes pass the container state as JSON on stdin.
			var state ociState
			if err := json.NewDecoder(os.Stdin).Decode(&state); err != nil {
				return fmt.Errorf("decode OCI state from stdin: %w", err)
			}
			if state.Pid == 0 {
				return fmt.Errorf("container PID is 0 — cannot move netdev")
			}

			logger.Info("CDI hook: moving netdev into container netns", "ibDev", ibDevName, "containerPID", state.Pid)
			return ib.MoveNetdevHookHelper(ctx, ibDevName, state.Pid)
		},
	}
}

func RunPlugin(ctx context.Context, config *Config) error {
	logger := klog.FromContext(ctx)

	err := os.MkdirAll(config.DriverPluginPath(), 0750)
	if err != nil {
		return err
	}

	info, err := os.Stat(config.flags.cdiRoot)
	switch {
	case err != nil && os.IsNotExist(err):
		err := os.MkdirAll(config.flags.cdiRoot, 0750)
		if err != nil {
			return err
		}
	case err != nil:
		return err
	case !info.IsDir():
		return fmt.Errorf("path for cdi file generation is not a directory: '%v'", err)
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer stop()
	ctx, cancel := context.WithCancelCause(ctx)
	config.cancelMainCtx = cancel

	driver, err := NewDriver(ctx, config)
	if err != nil {
		return err
	}

	<-ctx.Done()
	// restore default signal behavior as soon as possible in case graceful
	// shutdown gets stuck.
	stop()
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		// A canceled context is the normal case here when the process receives
		// a signal. Only log the error for more interesting cases.
		logger.Error(err, "error from context")
	}

	err = driver.Shutdown(logger)
	if err != nil {
		logger.Error(err, "Unable to cleanly shutdown driver")
	}

	return nil
}
