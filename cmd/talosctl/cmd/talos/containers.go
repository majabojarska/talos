// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package talos

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/siderolabs/talos/pkg/machinery/api/common"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/client/multiplex"
	"github.com/siderolabs/talos/pkg/machinery/constants"
)

// resolveContainerNamespace maps the -k / --namespace flags onto a containerd namespace and driver.
//
// The CRI driver only exists for Kubernetes pods; everything else, including the taloscontainers
// namespace, is read through the containerd driver. The server picks the right socket from the
// namespace, so no extra plumbing is needed there.
func resolveContainerNamespace(kubernetes bool, namespace string) (string, common.ContainerDriver, error) {
	switch {
	case kubernetes && namespace != "":
		return "", 0, errors.New("--kubernetes and --namespace are mutually exclusive")
	case kubernetes:
		return constants.K8sContainerdNamespace, common.ContainerDriver_CRI, nil
	case namespace == constants.K8sContainerdNamespace:
		return namespace, common.ContainerDriver_CRI, nil
	case namespace != "":
		return namespace, common.ContainerDriver_CONTAINERD, nil
	default:
		return constants.SystemContainerdNamespace, common.ContainerDriver_CONTAINERD, nil
	}
}

var containersCmdFlags struct {
	kubernetesNamespaceFlag

	namespace string
}

// containersCmd represents the processes command.
var containersCmd = &cobra.Command{
	Use:     "containers",
	Aliases: []string{"c"},
	Short:   "List containers",
	Long:    ``,
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		clientFactory, err := NewClientFactory(ctx, &containersCmdFlags)
		if err != nil {
			return err
		}

		defer clientFactory.Close() //nolint:errcheck

		namespace, driver, err := resolveContainerNamespace(containersCmdFlags.kubernetes, containersCmdFlags.namespace)
		if err != nil {
			return err
		}

		responseChan := multiplex.UnaryViaFactory(
			ctx, clientFactory,
			func(ctx context.Context, c *client.Client) (*machineapi.ContainersResponse, error) {
				return c.Containers(ctx, namespace, driver)
			},
		)

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, "NODE\tNAMESPACE\tID\tIMAGE\tPID\tSTATUS")

		flushTimer := time.NewTimer(outputFlushInterval)
		defer flushTimer.Stop()

		flushTimer.Stop()

		var errs error

		for {
			select {
			case resp, ok := <-responseChan:
				if !ok {
					return errors.Join(errs, w.Flush())
				}

				if resp.Err != nil {
					errs = errors.Join(errs, fmt.Errorf("error from node %s: %w", resp.Node, resp.Err))
				} else {
					for _, msg := range resp.Payload.Messages {
						slices.SortFunc(msg.Containers, func(a, b *machineapi.ContainerInfo) int { return strings.Compare(a.Id, b.Id) })

						for _, p := range msg.Containers {
							display := p.Id
							if p.Id != p.PodId {
								// container in a sandbox
								display = "└─ " + display
							}

							fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", resp.Node, p.Namespace, display, p.Image, p.Pid, p.Status)
						}
					}
				}

				flushTimer.Reset(outputFlushInterval)
			case <-flushTimer.C:
				if err := w.Flush(); err != nil {
					errs = errors.Join(errs, fmt.Errorf("error flushing output: %w", err))
				}
			}
		}
	},
}

func init() {
	containersCmd.Flags().BoolVarP(&containersCmdFlags.kubernetes, "kubernetes", "k", false, "use the k8s.io containerd namespace")
	containersCmd.Flags().StringVar(&containersCmdFlags.namespace, "namespace", "", "containerd namespace to use (e.g. taloscontainers)")

	containersCmd.Flags().Bool("use-cri", false, "use the CRI driver")
	containersCmd.Flags().MarkHidden("use-cri") //nolint:errcheck

	addCommand(containersCmd)
}
