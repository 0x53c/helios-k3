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

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		conf := config.New(ctx, "")
		sshUser := conf.Require("ssh_user")
		masterMacName := conf.Require("master_mac_name")
		masterMacIP := conf.Require("master_mac_ip")
		clusterName := conf.Get("cluster_name")
		if clusterName == "" {
			clusterName = "my-cluster"
		}

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

		// Provision master node with k3d and OrbStack
		masterNode, err := remote.NewCommand(ctx, "provision-master-node", &remote.CommandArgs{
			Connection: masterMacConnection,
			Create: pulumi.Sprintf(`
				set -e
				echo "Checking if OrbStack is running..." >&2
				if ! pgrep -q OrbStack; then
					echo "Starting OrbStack..." >&2
					open -a OrbStack
					sleep 10  # Give OrbStack time to initialize
				fi

				echo "Verifying Docker is functional via OrbStack..." >&2
				docker ps >/dev/null 2>&1 || { echo "Docker not running properly through OrbStack"; exit 1; }

				echo "Cleaning up any existing cluster..." >&2
				k3d cluster delete %[1]s >/dev/null 2>&1 || true

				echo "Creating k3d cluster on master node..." >&2
				k3d cluster create %[1]s \
					--api-port 6550 \
					--servers 1 \
					--agents 0 \
					--k3s-arg "--tls-san=%[2]s@server:0" \
					--port "80:80@loadbalancer" \
					--port "443:443@loadbalancer"

				echo "Setting up kubectl configuration..." >&2
				k3d kubeconfig get %[1]s > ~/.kube/config

				echo "Waiting for cluster to be ready..." >&2
				for i in {1..30}; do
					if kubectl get nodes >/dev/null 2>&1; then
						echo "Cluster is ready." >&2
						break
					fi
					if [ $i -eq 30 ]; then
						echo "Error: Timed out waiting for cluster to be ready." >&2
						exit 1
					fi
					echo "Still waiting... (attempt $i/30)" >&2
					sleep 5
				done

				echo "Extracting node token..." >&2
				NODE_TOKEN=$(docker exec k3d-%[1]s-server-0 cat /var/lib/rancher/k3s/server/node-token)
				
				echo "Extracting kubeconfig..." >&2
				KUBECONFIG=$(k3d kubeconfig get %[1]s | sed 's/0.0.0.0/%[2]s/g')

				jq -n \
					--arg ip '%[2]s' \
					--arg token "$NODE_TOKEN" \
					--arg kubeconfig "$KUBECONFIG" \
					'{ip: $ip, token: $token, kubeconfig: $kubeconfig}'
			`, clusterName, masterMacIP),
			Delete: pulumi.Sprintf("k3d cluster delete %s || true", clusterName),
		}, pulumi.Timeouts(&pulumi.CustomTimeouts{Create: "15m"}))
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

		// Provision worker nodes
		for i, worker := range workerNodes {
			worker := worker
			workerNumber := i + 1

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

			createScriptAnyOutput := pulumi.All(masterIP, masterToken, clusterName).ApplyT(func(args []interface{}) (string, error) {
				masterIPStr := args[0].(string)
				masterTokenStr := args[1].(string)
				clusterNameStr := args[2].(string)

				script := fmt.Sprintf(`
					set -e
					echo "Checking if OrbStack is running..." >&2
					if ! pgrep -q OrbStack; then
						echo "Starting OrbStack..." >&2
						open -a OrbStack
						sleep 10  # Give OrbStack time to initialize
					fi

					echo "Verifying Docker is functional via OrbStack..." >&2
					docker ps >/dev/null 2>&1 || { echo "Docker not running properly through OrbStack"; exit 1; }

					echo "Creating agent node with k3d..." >&2
					k3d node delete agent-%[1]d >/dev/null 2>&1 || true
					k3d node create agent-%[1]d \
						--cluster %[4]s \
						--k3s-arg "--token=%[3]s@agent:0" \
						--k3s-arg "--server=https://%[2]s:6550@agent:0"

					# Wait for node to be ready
					echo "Waiting for agent node to join the cluster..." >&2
					for i in {1..30}; do
						if docker ps | grep -q "k3d-agent-%[1]d"; then
							echo "Agent node has started successfully." >&2
							break
						fi
						if [ $i -eq 30 ]; then
							echo "Error: Timed out waiting for agent node container to start." >&2
							exit 1
						fi
						echo "Still waiting for container to start... (attempt $i/30)" >&2
						sleep 5
					done

					# Get the worker's IP address
					WORKER_IP=$(ifconfig | grep "inet " | grep -v 127.0.0.1 | awk '{print $2}' | head -n 1)
					
					jq -n --arg ip "$WORKER_IP" '{ip: $ip}'
				`, workerNumber, masterIPStr, masterTokenStr, clusterNameStr)

				return script, nil
			})

			workerNode, err := remote.NewCommand(ctx, fmt.Sprintf("provision-worker-%s", worker.Name), &remote.CommandArgs{
				Connection: workerMacConnection,
				Create:     pulumi.Sprintf("%s", createScriptAnyOutput),
				Delete:     pulumi.Sprintf("k3d node delete agent-%d || true", workerNumber),
			}, pulumi.DependsOn([]pulumi.Resource{masterNode}), pulumi.Timeouts(&pulumi.CustomTimeouts{Create: "15m"}))
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
