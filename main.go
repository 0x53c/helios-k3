package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/pulumi/pulumi-command/sdk/go/command/remote"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

type WorkerNodeConfig struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
}

const brewBinPath = "/opt/homebrew/bin"

const masterLimaConfig = `
vmType: "vz"
images:
- location: "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-arm64.img"
  arch: "aarch64"
mounts:
- location: "~"
- location: "/tmp/lima"
  writable: true
portForwards:
- guestPort: 6443
  hostIP: "0.0.0.0" 
  hostPort: 6443
`

const workerLimaConfig = `
vmType: "vz"
images:
- location: "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-arm64.img"
  arch: "aarch64"
mounts:
- location: "~"
- location: "/tmp/lima"
  writable: true
`

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		// --- 1. Load Configuration (No Changes) ---
		conf := config.New(ctx, "")
		sshUser := conf.Require("ssh_user")
		masterMacName := conf.Require("master_mac_name")
		masterMacIP := conf.Require("master_mac_ip")

		var workerNodes []WorkerNodeConfig
		workerNodesJSON := conf.Require("worker_nodes")
		if err := json.Unmarshal([]byte(workerNodesJSON), &workerNodes); err != nil {
			return fmt.Errorf("failed to unmarshal worker_nodes config: %w", err)
		}

		masterKeyEnvVar := fmt.Sprintf("%s_PRIVATE_KEY", strings.ToUpper(masterMacName))
		masterPrivateKeyStr := os.Getenv(masterKeyEnvVar)
		if masterPrivateKeyStr == "" {
			return fmt.Errorf("environment variable %s is not set", masterKeyEnvVar)
		}
		masterSshPrivateKey := pulumi.ToSecret(pulumi.String(masterPrivateKeyStr)).(pulumi.StringOutput)

		masterMacConnection := remote.ConnectionArgs{
			Host:       pulumi.String(masterMacIP),
			User:       pulumi.String(sshUser),
			PrivateKey: masterSshPrivateKey,
		}

		masterNode, err := remote.NewCommand(ctx, "provision-master-node", &remote.CommandArgs{
			Connection: masterMacConnection,
			Create: pulumi.Sprintf(`
				set -e
				echo "Deleting old master instance..." >&2
				%[1]s/limactl delete -f k3s-master > /dev/null || true
				echo "Clearing Lima image cache..." >&2
				%[1]s/limactl cache delete > /dev/null || true
				echo "Writing Lima config for master..." >&2
				printf -- '%[3]s' > /tmp/k3s-master.yaml
				echo "Starting k3s master node..." >&2
				%[1]s/limactl start --name=k3s-master /tmp/k3s-master.yaml > /dev/null

				echo "Waiting for master VM to be ready..." >&2
				for i in {1..25}; do
					if %[1]s/limactl shell k3s-master echo "ready" >/dev/null 2>&1; then
						echo "Master VM is ready." >&2; break
					fi
					if [ $i -eq 25 ]; then echo "Error: Timed out waiting for master VM." >&2; exit 1; fi
					echo "Still waiting... (attempt $i/25)" >&2; sleep 8
				done

				echo "Provisioning k3s and extracting details..." >&2
				%[1]s/limactl shell k3s-master -- bash -c "
					set -e
					# FINAL FIX: Add --flannel-backend=wireguard-native to enable cluster networking over the LAN.
					curl -sfL https://get.k3s.io | sh -s - server --tls-san %[2]s --flannel-backend=wireguard-native &> /tmp/k3s-install.log
					
					echo 'Waiting for k3s secrets...' >&2
					counter=0
					while ! sudo test -f /var/lib/rancher/k3s/server/node-token || ! sudo test -f /etc/rancher/k3s/k3s.yaml; do
						if [ \$counter -ge 90 ]; then
							echo 'Error: Timed out waiting for k3s secret files.' >&2
							cat /tmp/k3s-install.log >&2
							exit 1
						fi
						counter=\$((counter+1))
						sleep 2
					done
					echo 'k3s secrets found.' >&2

					TOKEN=\$(sudo cat /var/lib/rancher/k3s/server/node-token)
					KUBECONFIG=\$(sudo cat /etc/rancher/k3s/k3s.yaml | sed 's/127.0.0.1/%[2]s/g')

					jq -n \
					  --arg ip '%[2]s' \
					  --arg token \"\$TOKEN\" \
					  --arg kubeconfig \"\$KUBECONFIG\" \
					  '{ip: \$ip, token: \$token, kubeconfig: \$kubeconfig}'
				"
			`, brewBinPath, masterMacIP, masterLimaConfig),
			Delete: pulumi.Sprintf("%s/limactl delete -f k3s-master", brewBinPath),
		}, pulumi.Timeouts(&pulumi.CustomTimeouts{Create: "30m"}))
		if err != nil {
			return err
		}

		masterOutput := masterNode.Stdout.ApplyT(func(output string) (map[string]interface{}, error) {
			var result map[string]interface{}
			err := json.Unmarshal([]byte(output), &result)
			if err != nil {
				return nil, fmt.Errorf("failed to unmarshal master output. Raw output was: %s. Error: %w", output, err)
			}
			return result, nil
		}).(pulumi.MapOutput)

		masterIP := pulumi.Sprintf("%s", masterOutput.MapIndex(pulumi.String("ip")))
		masterToken := pulumi.Sprintf("%s", masterOutput.MapIndex(pulumi.String("token")))
		kubeConfig := pulumi.Sprintf("%s", masterOutput.MapIndex(pulumi.String("kubeconfig")))

		var workerNodeIPs []pulumi.StringInput

		for _, worker := range workerNodes {
			worker := worker
			limaWorkerName := fmt.Sprintf("k3s-%s", strings.ToLower(worker.Name))

			workerKeyEnvVar := fmt.Sprintf("%s_PRIVATE_KEY", strings.ToUpper(worker.Name))
			workerPrivateKeyStr := os.Getenv(workerKeyEnvVar)
			if workerPrivateKeyStr == "" {
				return fmt.Errorf("environment variable %s is not set", workerKeyEnvVar)
			}
			workerSshPrivateKey := pulumi.ToSecret(pulumi.String(workerPrivateKeyStr)).(pulumi.StringOutput)

			workerMacConnection := remote.ConnectionArgs{
				Host:       pulumi.String(worker.IP),
				User:       pulumi.String(sshUser),
				PrivateKey: workerSshPrivateKey,
			}

			createScriptAnyOutput := pulumi.All(masterIP, masterToken).ApplyT(func(args []interface{}) (string, error) {
				masterIPStr := args[0].(string)
				masterTokenStr := args[1].(string)
				script := fmt.Sprintf(`
					set -e
					echo "Deleting old worker instance %[1]s..." >&2
					%[3]s/limactl delete -f %[1]s > /dev/null || true
					echo "Clearing Lima image cache on worker..." >&2
					%[3]s/limactl cache delete > /dev/null || true
					echo "Writing Lima config for worker %[1]s..." >&2
					printf -- '%[5]s' > /tmp/%[1]s.yaml
					echo "Starting worker node %[1]s..." >&2
					%[3]s/limactl start --name=%[1]s /tmp/%[1]s.yaml > /dev/null
					
					echo "Waiting for worker VM %[1]s to be ready..." >&2
					for i in {1..25}; do
						if %[3]s/limactl shell %[1]s echo "ready" >/dev/null 2>&1; then
							echo "Worker VM %[1]s is ready." >&2; break
						fi
						if [ $i -eq 25 ]; then echo "Error: Timed out waiting for worker VM %[1]s." >&2; exit 1; fi
						echo "Still waiting... (attempt $i/25)" >&2; sleep 8
					done

					echo "Joining worker %[1]s to the cluster..." >&2
					%[3]s/limactl shell %[1]s -- bash -c "
						curl -sfL https://get.k3s.io | K3S_URL=https://%[2]s:6443 K3S_TOKEN=\"%[4]s\" sh -s - &> /tmp/k3s-join.log
					"
					
					INTERNAL_IP=\$(%[3]s/limactl list %[1]s --json | %[3]s/jq -r .address)
					# FINAL FIX: Removed the unnecessary backslash from '$ip'
					%[3]s/jq -n --arg ip "\$INTERNAL_IP" '{ip: $ip}'
				`, limaWorkerName, masterIPStr, brewBinPath, masterTokenStr, workerLimaConfig)
				return script, nil
			})

			workerNode, err := remote.NewCommand(ctx, fmt.Sprintf("provision-worker-%s", worker.Name), &remote.CommandArgs{
				Connection: workerMacConnection,
				Create:     pulumi.Sprintf("%s", createScriptAnyOutput),
				Delete:     pulumi.Sprintf("%s/limactl delete -f %s", brewBinPath, limaWorkerName),
			}, pulumi.DependsOn([]pulumi.Resource{masterNode}), pulumi.Timeouts(&pulumi.CustomTimeouts{Create: "25m"}))
			if err != nil {
				return err
			}

			workerOutput := workerNode.Stdout.ApplyT(func(output string) (map[string]interface{}, error) {
				var result map[string]interface{}
				json.Unmarshal([]byte(output), &result)
				return result, nil
			}).(pulumi.MapOutput)

			workerNodeIPs = append(workerNodeIPs, pulumi.Sprintf("%s", workerOutput.MapIndex(pulumi.String("ip"))))
		}

		allWorkerIPs := make([]interface{}, len(workerNodeIPs))
		for i, ip := range workerNodeIPs {
			allWorkerIPs[i] = ip
		}

		ctx.Export("masterNodeIP", masterIP)
		ctx.Export("workerNodeIPs", pulumi.All(allWorkerIPs...))
		ctx.Export("kubeconfig", pulumi.ToSecret(kubeConfig))

		return nil
	})
}
