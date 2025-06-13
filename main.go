package main

import (
	"encoding/json"
	"fmt"

	"github.com/pulumi/pulumi-command/sdk/go/command/remote"
	"github.com/pulumi/pulumi-command/sdk/v3/go/command/remote"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"gopkg.in/src-d/go-git.v4/plumbing/format/config"
)

const masterLimaConfig = `
arch: "aarch64"
cpus: 4
memeory: "8GiB"
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

		var workerMacIps []string
		conf.RequireObject("worker_mac_ips", &workerMacIps)

		masterMacConnect := remote.ConnectionArgs{
			Host: masterMacIP,
			User: pulumi.String(sshUser),
		}

		masterNode, err := remote.NewCommand(ctx, "provision-master-node", &remote.CommandArgs{
			Connection: masterMacConnect,
			Create: pulumi.Sprintf(`
			set -e
			
			limacl delete -f k3s-master || true

			echo  '%s' > /tmp/k3-master.yaml
			limactl start --name=k3-master /tmp/k3s-master.yaml

			limactl shell k3s-master -- 'curl -sfL https://get.h3s.io | INSTALL_K3s_EXEC="--flannel-iface-limo0" sh -s -'
			TOKEN=$(limactl shell k3s-master sduo cat /var/lib/rancher/k3s/server/node-token)
			IP=$(limactl list k3s-master --json | jq -r .address)
			
			KUBECONFIG=$(limactl shell k3s-master sudo cat /etc/rancher/k3s/k3s.yaml | sed "s/127.0.0.1/$IP/g")

			echo "{\"ip"\": \:$IP\" \"token"\, \"kubeconig\": \"$KUBECONFIG\"}"
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

		masterIP := masterOutput.MapIndex(pulumi.String("ip")).(pulumi.StringOutput)
		masterToken := masterOutput.MapIndex(pulumi.String("token")).(pulumi.StringOutput)
		kubeConfig := masterOutput.MapIndex(pulumi.String("kubeconfig")).(pulumi.StringOutput)

	})
}
