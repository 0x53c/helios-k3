package main

import (
	"encoding/json"
	"fmt"

	"github.com/pulumi/pulumi-command/sdk/go/command/remote"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

const masterLimaConfig = `
arch: "aarch64"
cpus: 4
memory: "8GiB"
disk: "60GiB"
`
const workerLimaConfig = `
arch: "aarch64"
cpus: 2
memory: "4GiB"
disk: "40GiB"
`

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		conf := config.New(ctx, "")
		sshUser := conf.Require("ssh_user")
		masterMacIP := conf.Require("master_mac_ip")

		var workerMacIPs []string
		conf.RequireObject("worker_mac_ips", &workerMacIPs)
		masterMacConnection := remote.ConnectionArgs{
			Host: pulumi.String(masterMacIP),
			User: pulumi.String(sshUser),
		}

		masterNode, err := remote.NewCommand(ctx, "provision-master-node", &remote.CommandArgs{
			Connection: masterMacConnection,
			Create: pulumi.Sprintf(`
				set -e
				limactl delete -f k3s-master || true
				echo '%s' > /tmp/k3s-master.yaml
				limactl start --name=k3s-master /tmp/k3s-master.yaml
				limactl shell k3s-master -- 'curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="--flannel-iface=lima0" sh -s -'
				TOKEN=$(limactl shell k3s-master sudo cat /var/lib/rancher/k3s/server/node-token)
				IP=$(limactl list k3s-master --json | jq -r .address)
				KUBECONFIG=$(limactl shell k3s-master sudo cat /etc/rancher/k3s/k3s.yaml | sed "s/127.0.0.1/$IP/g")
				echo "{\"ip\": \"$IP\", \"token\": \"$TOKEN\", \"kubeconfig\": \"$KUBECONFIG\"}"
			`, masterLimaConfig),
			Delete: pulumi.String("limactl delete -f k3s-master"),
		}, pulumi.Timeouts(&pulumi.CustomTimeouts{Create: "20m"}))
		if err != nil {
			return err
		}

		masterOutput := masterNode.Stdout.ApplyT(func(output string) (map[string]interface{}, error) {
			var result map[string]interface{}
			err := json.Unmarshal([]byte(output), &result)
			if err != nil {
				return nil, fmt.Errorf("failed to unmarshal master output: %w", err)
			}
			return result, nil
		}).(pulumi.MapOutput)

		masterIP := pulumi.Sprintf("%s", masterOutput.MapIndex(pulumi.String("ip")))
		masterToken := pulumi.Sprintf("%s", masterOutput.MapIndex(pulumi.String("token")))
		kubeConfig := pulumi.Sprintf("%s", masterOutput.MapIndex(pulumi.String("kubeconfig")))

		var workerNodeIPs []pulumi.StringInput

		for i, workerMacIP := range workerMacIPs {
			workerName := fmt.Sprintf("k3s-worker-%d", i)

			workerMacConnection := remote.ConnectionArgs{
				Host: pulumi.String(workerMacIP),
				User: pulumi.String(sshUser),
			}

			createScriptAnyOutput := pulumi.All(masterIP, masterToken).ApplyT(func(args []interface{}) (string, error) {
				masterIPStr := args[0].(string)
				masterTokenStr := args[1].(string)

				script := fmt.Sprintf(`
					set -e
					limactl delete -f %s || true
					echo '%s' > /tmp/%s.yaml
					limactl start --name=%s /tmp/%s.yaml
					limactl shell %s -- 'curl -sfL https://get.k3s.io | K3S_URL=https://%s:6443 K3S_TOKEN="%s" INSTALL_K3S_EXEC="--flannel-iface=lima0" sh -'
					IP=$(limactl list %s --json | jq -r .address)
					echo "{\"ip\": \"$IP\"}"
				`, workerName, workerLimaConfig, workerName, workerName, workerName, workerName, masterIPStr, masterTokenStr, workerName)

				return script, nil
			})

			workerNode, err := remote.NewCommand(ctx, fmt.Sprintf("provision-worker-%d", i), &remote.CommandArgs{
				Connection: workerMacConnection,
				Create:     pulumi.Sprintf("%s", createScriptAnyOutput),
				Delete:     pulumi.Sprintf("limactl delete -f %s", workerName),
			}, pulumi.DependsOn([]pulumi.Resource{masterNode}), pulumi.Timeouts(&pulumi.CustomTimeouts{Create: "20m"}))
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
