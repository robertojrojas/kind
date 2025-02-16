/*
Copyright 2019 The Kubernetes Authors.

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

// Package config implements the kubeadm config action
package config

import (
	"strings"

	"sigs.k8s.io/kind/pkg/cluster/constants"
	"sigs.k8s.io/kind/pkg/cluster/nodes"
	"sigs.k8s.io/kind/pkg/errors"

	"sigs.k8s.io/kind/pkg/cluster/nodeutils"
	"sigs.k8s.io/kind/pkg/internal/apis/config"
	"sigs.k8s.io/kind/pkg/internal/cluster/create/actions"
	"sigs.k8s.io/kind/pkg/internal/cluster/kubeadm"
	"sigs.k8s.io/kind/pkg/internal/cluster/providers/provider/common"
	"sigs.k8s.io/kind/pkg/internal/util/patch"
)

// Action implements action for creating the kubeadm config
// and deployng it on the bootrap control-plane node.
type Action struct{}

// NewAction returns a new action for creating the kubadm config
func NewAction() actions.Action {
	return &Action{}
}

// Execute runs the action
func (a *Action) Execute(ctx *actions.ActionContext) error {
	ctx.Status.Start("Creating kubeadm config 📜")
	defer ctx.Status.End(false)

	allNodes, err := ctx.Nodes()
	if err != nil {
		return err
	}

	// get the control plane endpoint, in case the cluster has an external load balancer in
	// front of the control-plane nodes
	controlPlaneEndpoint, controlPlaneEndpointIPv6, err := nodeutils.GetControlPlaneEndpoint(allNodes)
	if err != nil {
		// TODO(bentheelder): logging here
		return err
	}

	// configure the right protocol addresses
	if ctx.Config.Networking.IPFamily == "ipv6" {
		controlPlaneEndpoint = controlPlaneEndpointIPv6
	}

	// create kubeadm init config
	fns := []func() error{}

	configData := kubeadm.ConfigData{
		ClusterName:          ctx.ClusterContext.Name(),
		ControlPlaneEndpoint: controlPlaneEndpoint,
		APIBindPort:          common.APIServerInternalPort,
		APIServerAddress:     ctx.Config.Networking.APIServerAddress,
		Token:                kubeadm.Token,
		PodSubnet:            ctx.Config.Networking.PodSubnet,
		ServiceSubnet:        ctx.Config.Networking.ServiceSubnet,
		ControlPlane:         true,
		IPv6:                 ctx.Config.Networking.IPFamily == "ipv6",
	}

	kubeadmConfigPlusPatches := func(node nodes.Node, data kubeadm.ConfigData) func() error {
		return func() error {
			kubeadmConfig, err := getKubeadmConfig(ctx.Config, data, node)
			if err != nil {
				// TODO(bentheelder): logging here
				return errors.Wrap(err, "failed to generate kubeadm config content")
			}

			ctx.Logger.V(2).Info("Using kubeadm config:\n" + kubeadmConfig)
			return writeKubeadmConfig(kubeadmConfig, node)
		}
	}

	// create the kubeadm join configuration for control plane nodes
	controlPlanes, err := nodeutils.ControlPlaneNodes(allNodes)
	if err != nil {
		return err
	}

	for _, node := range controlPlanes {
		node := node             // capture loop variable
		configData := configData // copy config data
		fns = append(fns, kubeadmConfigPlusPatches(node, configData))
	}

	// then create the kubeadm join config for the worker nodes if any
	workers, err := nodeutils.SelectNodesByRole(allNodes, constants.WorkerNodeRoleValue)
	if err != nil {
		return err
	}
	if len(workers) > 0 {
		// create the workers concurrently
		for _, node := range workers {
			node := node             // capture loop variable
			configData := configData // copy config data
			configData.ControlPlane = false
			fns = append(fns, kubeadmConfigPlusPatches(node, configData))
		}
	}

	// Create the config in all nodes concurrently
	if err := errors.UntilErrorConcurrent(fns); err != nil {
		return err
	}

	// mark success
	ctx.Status.End(true)
	return nil
}

// getKubeadmConfig generates the kubeadm config contents for the cluster
// by running data through the template and applying patches as needed.
func getKubeadmConfig(cfg *config.Cluster, data kubeadm.ConfigData, node nodes.Node) (path string, err error) {
	kubeVersion, err := nodeutils.KubeVersion(node)
	if err != nil {
		// TODO(bentheelder): logging here
		return "", errors.Wrap(err, "failed to get kubernetes version from node")
	}
	data.KubernetesVersion = kubeVersion

	// get the node ip address
	nodeAddress, nodeAddressIPv6, err := node.IP()
	if err != nil {
		return "", errors.Wrap(err, "failed to get IP for node")
	}

	data.NodeAddress = nodeAddress
	// configure the right protocol addresses
	if cfg.Networking.IPFamily == "ipv6" {
		data.NodeAddress = nodeAddressIPv6
	}

	// generate the config contents
	cf, err := kubeadm.Config(data)
	if err != nil {
		return "", err
	}

	clusterPatches, clusterJSONPatches := allPatchesFromConfig(cfg)
	// apply cluster-level patches first
	patchedConfig, err := patch.Patch(cf, clusterPatches, clusterJSONPatches)
	if err != nil {
		return "", err
	}

	// since we only need the last portion of the name,
	// create namer without a clusterName
	namer := common.MakeNodeNamer("")
	for _, inode := range cfg.Nodes {
		nodeSuffix := namer(string(inode.Role))
		// if needed, apply current node's patches
		if strings.HasSuffix(node.String(), nodeSuffix) && (len(inode.KubeadmConfigPatches) > 0 || len(inode.KubeadmConfigPatchesJSON6902) > 0) {
			patchedConfig, err = patch.Patch(patchedConfig, inode.KubeadmConfigPatches, inode.KubeadmConfigPatchesJSON6902)
			if err != nil {
				return "", err
			}
		}
	}

	// fix all the patches to have name metadata matching the generated config
	return removeMetadata(patchedConfig), nil
}

// trims out the metadata.name we put in the config for kustomize matching,
// kubeadm will complain about this otherwise
func removeMetadata(kustomized string) string {
	return strings.Replace(
		kustomized,
		`metadata:
  name: config
`,
		"",
		-1,
	)
}

func allPatchesFromConfig(cfg *config.Cluster) (patches []string, jsonPatches []config.PatchJSON6902) {
	return cfg.KubeadmConfigPatches, cfg.KubeadmConfigPatchesJSON6902
}

// writeKubeadmConfig writes the kubeadm configuration in the specified node
func writeKubeadmConfig(kubeadmConfig string, node nodes.Node) error {
	// copy the config to the node
	if err := nodeutils.WriteFile(node, "/kind/kubeadm.conf", kubeadmConfig); err != nil {
		// TODO(bentheelder): logging here
		return errors.Wrap(err, "failed to copy kubeadm config to node")
	}

	return nil
}
