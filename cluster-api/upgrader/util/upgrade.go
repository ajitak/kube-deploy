/*
Copyright 2017 The Kubernetes Authors.

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
package util

import (
	"fmt"
	"time"

	"github.com/golang/glog"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "k8s.io/kube-deploy/cluster-api/api/cluster/v1alpha1"
	clusapiclnt "k8s.io/kube-deploy/cluster-api/client"
	"k8s.io/kube-deploy/cluster-api/util"
)

var (
	kubeClientSet *kubernetes.Clientset
	client        *clusapiclnt.ClusterAPIV1Alpha1Client
)

func initClient(kubeconfig string) error {
	if kubeconfig == "" {
		kubeconfig = util.GetDefaultKubeConfigPath()
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		glog.Fatalf("BuildConfigFromFlags failed: %v", err)
		return err
	}

	kubeClientSet, err = kubernetes.NewForConfig(config)
	if err != nil {
		glog.Fatalf("error creating kube client set: %v", err)
	}

	client, err = clusapiclnt.NewForConfig(config)
	if err != nil {
		glog.Fatalf("error creating cluster api client: %v", err)
		return err
	}

	return nil
}

func checkMachineReady(machineName string, kubeVersion string) (bool, error) {
	machine, err := client.Machines().Get(machineName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	// Find the node object via reference in machine object.
	node, err := kubeClientSet.CoreV1().Nodes().Get(machine.Status.NodeRef.Name, metav1.GetOptions{})
	switch {
	case err != nil:
		glog.Infof("Failed to get node %s: %v", machineName, err)
		return false, err
	case !util.IsNodeReady(node):
		glog.Infof("node %s is not ready. Status : %v", machineName, node.Status.Conditions)
		return false, nil
	case node.Status.NodeInfo.KubeletVersion == "v"+kubeVersion:
		glog.Infof("node %s is ready", machineName)
		return true, nil
	default:
		glog.Infof("node %s kubelet current version: %s, target: %s.", machineName, node.Status.NodeInfo.KubeletVersion, kubeVersion)
		return false, nil
	}
}

func UpgradeCluster(kubeversion string, kubeconfig string) error {
	glog.Infof("Starting to upgrade cluster to version: %s", kubeversion)

	if err := initClient(kubeconfig); err != nil {
		return err
	}

	machine_list, err := client.Machines().List(metav1.ListOptions{})
	if err != nil {
		return err
	}

	glog.Info("Upgrading the control plane.")

	// Update the control plan first. It assumes single master.
	master_found := false
	for _, mach := range machine_list.Items {
		if util.IsMaster(&mach) {
			mach.Spec.Versions.Kubelet = kubeversion
			mach.Spec.Versions.ControlPlane = kubeversion
			new_machine, err := client.Machines().Update(&mach)
			if err == nil {
				err = wait.Poll(5*time.Second, 10*time.Minute, func() (bool, error) {
					ready, err := checkMachineReady(new_machine.Name, kubeversion)
					if err != nil {
						// Ignore the error as control plan is restarting.
						return false, nil
					}
					return ready, nil
				})
			}
			master_found = true
			break
		}
	}

	if err == nil && !master_found {
		err = fmt.Errorf("No master is found.")
	}
	if err != nil {
		return err
	}

	glog.Info("Upgrading the nodes.")

	// Continue to update all the nodes.
	errors := make(chan error, len(machine_list.Items))
	for i, _ := range machine_list.Items {
		if !util.IsMaster(&machine_list.Items[i]) {
			go func(mach *clusterv1.Machine) {
				mach, err = client.Machines().Get(mach.Name, metav1.GetOptions{})
				if err == nil {
					mach.Spec.Versions.Kubelet = kubeversion
					new_machine, err := client.Machines().Update(mach)
					if err == nil {
						// Polling the cluster until nodes are updated.
						err = wait.Poll(5*time.Second, 10*time.Minute, func() (bool, error) {
							ready, err := checkMachineReady(new_machine.Name, kubeversion)
							if err != nil {
								// Ignore the error as control plan is restarting.
								return false, nil
							}
							return ready, nil
						})
					} else {
						glog.Errorf("Update to machine object (%s) failed : %v", mach.Name, err)
					}
				} else {
					glog.Errorf("client.Machines().Get() failed : %v", err)
				}
				errors <- err
			}(&machine_list.Items[i])
		}
	}
	glog.Info("Waiting for update to be completed.")

	for _, machine := range machine_list.Items {
		if !util.IsMaster(&machine) {
			if err = <-errors; err != nil {
				return err
			}
		}
	}

	glog.Info("Successfully upgraded the cluster.")
	return nil
}
