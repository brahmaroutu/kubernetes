/*
Copyright 2018 The Kubernetes Authors.

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

package storage

import (
	"fmt"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	apps "k8s.io/api/apps/v1"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/uuid"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/storage/utils"
	"path"
)

var _ = utils.SIGDescribe("FlexResize [Slow]", func() {
	var (
		cs                clientset.Interface
		ns                string
		err               error
		pv                *v1.PersistentVolume
		pvc               *v1.PersistentVolumeClaim
		nodeName          string
		isNodeLabeled     bool
		nodeKeyValueLabel map[string]string
		nodeLabelValue    string
		nodeKey           string
		node              v1.Node
		config            framework.VolumeTestConfig
		suffix            string
	)

	f := framework.NewDefaultFramework("flexvolume-expand")
	BeforeEach(func() {
		framework.SkipUnlessProviderIs("aws", "gce", "local")
		cs = f.ClientSet
		ns = f.Namespace.Name
		framework.ExpectNoError(framework.WaitForAllNodesSchedulable(cs, framework.TestContext.NodeSchedulableTimeout))

		nodeList := framework.GetReadySchedulableNodesOrDie(f.ClientSet)
		if len(nodeList.Items) != 0 {
			nodeName = nodeList.Items[0].Name
		} else {
			framework.Failf("Unable to find ready and schedulable Node")
		}

		nodeKey = "mounted_volume_expand"
		node = nodeList.Items[0]

		if !isNodeLabeled {
			nodeLabelValue = ns
			nodeKeyValueLabel = make(map[string]string)
			nodeKeyValueLabel[nodeKey] = nodeLabelValue
			framework.AddOrUpdateLabelOnNode(cs, nodeName, nodeKey, nodeLabelValue)
			isNodeLabeled = true
		}

		config = framework.VolumeTestConfig{
			Namespace:      ns,
			Prefix:         "flex",
			ClientNodeName: node.Name,
		}

		suffix = ns

	})

	framework.AddCleanupAction(func() {
		if len(nodeLabelValue) > 0 {
			framework.RemoveLabelOffNode(cs, nodeName, nodeKey)
		}
	})

	AfterEach(func() {
		framework.Logf("AfterEach: Cleaning up resources for mounted volume resize")

		if cs != nil {
			if errs := framework.PVPVCCleanup(cs, ns, pv, pvc); len(errs) > 0 {
				framework.Failf("AfterEach: Failed to delete PVC and/or PV. Errors: %v", utilerrors.NewAggregate(errs))
			}
			pvc, nodeName, isNodeLabeled, nodeLabelValue = nil, "", false, ""
			nodeKeyValueLabel = make(map[string]string)
		}
	})

	It("Should verify mounted devices can be resized", func() {
		driver := "dummy-attachable"
		driverInstallAs := driver + "-" + suffix

		By(fmt.Sprintf("installing flexvolume %s on node %s as %s", path.Join(driverDir, driver), node.Name, driverInstallAs))
		installFlex(cs, &node, "k8s", driverInstallAs, path.Join(driverDir, driver))

		pv = &v1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: "flex-volume-0",
				Annotations: map[string]string{
					"volume.beta.kubernetes.io/storage-class": "",
				},
			},
			Spec: v1.PersistentVolumeSpec{
				AccessModes: []v1.PersistentVolumeAccessMode{
					v1.ReadWriteOnce,
				},
				Capacity: v1.ResourceList{
					v1.ResourceName(v1.ResourceStorage): resource.MustParse("6Gi"),
				},
				PersistentVolumeSource: v1.PersistentVolumeSource{
					FlexVolume: &v1.FlexPersistentVolumeSource{
						Driver:   "k8s/" + driver,
						ReadOnly: true,
					},
				},
			},
		}
		pv, err = cs.CoreV1().PersistentVolumes().Create(pv)
		framework.ExpectNoError(err)

		pvc = &v1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-claim",
				Namespace: ns,
				Annotations: map[string]string{
					"volume.beta.kubernetes.io/storage-class": "",
				},
			},
			Spec: v1.PersistentVolumeClaimSpec{
				AccessModes: []v1.PersistentVolumeAccessMode{
					v1.ReadWriteOnce,
				},
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						"storage": resource.MustParse("2Gi"),
					},
				},
				VolumeName: "flex-volume-0",
			},
		}

		//pvc = newClaim(test, ns.Name, "default")
		pvc, err = cs.CoreV1().PersistentVolumeClaims(pvc.Namespace).Create(pvc)
		Expect(err).NotTo(HaveOccurred(), "Error creating pvc")

		pvcClaims := []*v1.PersistentVolumeClaim{pvc}
		pvs, err := framework.WaitForPVClaimBoundPhase(cs, pvcClaims, framework.ClaimProvisionShortTimeout)
		Expect(err).NotTo(HaveOccurred(), "Failed waiting for PVC to be bound %v", err)
		Expect(len(pvs)).To(Equal(1))
		By("Creating a deployment with the provisioned volume")
		framework.Logf("FLEXRESIZE PVC resize starting")
		deployment, err := CreateFlexDeployment(cs, int32(1), map[string]string{"test": "app"}, nodeKeyValueLabel, ns, pvcClaims, "")
		Expect(err).NotTo(HaveOccurred(), "Failed creating deployment %v", err)
		defer cs.AppsV1().Deployments(ns).Delete(deployment.Name, &metav1.DeleteOptions{})

		framework.Logf("FLEXRESIZE PVC initial ", pvc.Spec.Resources.Requests[v1.ResourceStorage])
		By("Expanding current pvc")
		newSize := resource.MustParse("4Gi")
		pvc, err = expandPVCSize(pvc, newSize, cs)
		Expect(err).NotTo(HaveOccurred(), "While updating pvc for more size")
		Expect(pvc).NotTo(BeNil())

		pvcSize := pvc.Spec.Resources.Requests[v1.ResourceStorage]
		if pvcSize.Cmp(newSize) != 0 {
			framework.Failf("error updating pvc size %q", pvc.Name)
		}
		framework.Logf("FLEXRESIZE PVC after ", pvc.Spec.Resources.Requests[v1.ResourceStorage])
		if len(pvc.Status.Conditions) >= 1 {
		framework.Logf("FLEXRESIZE PVC state after ", pvc.Status.Conditions[0].Status,"/",pvc.Status.Conditions[0].Reason,"/",pvc.Status.Conditions[0].Message)
		}
		//deployment, err = CreateFlexDeployment(cs, int32(1), map[string]string{"test": "app"}, nodeKeyValueLabel, ns, pvcClaims, "")
		//Expect(err).NotTo(HaveOccurred(), "Failed creating deployment %v", err)
		framework.Logf("SRINIB *****************", pvc.Spec.Resources.Requests[v1.ResourceStorage])
	})
})

func CreateFlexDeployment(client clientset.Interface, replicas int32, podLabels map[string]string, nodeSelector map[string]string, namespace string, pvclaims []*v1.PersistentVolumeClaim, command string) (*apps.Deployment, error) {
	deploymentSpec := MakeFlexDeployment(replicas, podLabels, nodeSelector, namespace, pvclaims, false, command)

	deployment, err := client.AppsV1().Deployments(namespace).Create(deploymentSpec)
	if err != nil {
		return nil, fmt.Errorf("deployment %q Create API error: %v", deploymentSpec.Name, err)
	}
	framework.Logf("Waiting deployment %q to complete", deploymentSpec.Name)
	err = framework.WaitForDeploymentComplete(client, deployment)
	if err != nil {
		return nil, fmt.Errorf("deployment %q failed to complete: %v", deploymentSpec.Name, err)
	}
	return deployment, nil
}

// MakeDeployment creates a deployment definition based on the namespace. The deployment references the PVC's
// name.  A slice of BASH commands can be supplied as args to be run by the pod
func MakeFlexDeployment(replicas int32, podLabels map[string]string, nodeSelector map[string]string, namespace string, pvclaims []*v1.PersistentVolumeClaim, isPrivileged bool, command string) *apps.Deployment {
	if len(command) == 0 {
		command = "trap exit TERM; while true; do sleep 1; done"
	}
	zero := int64(0)
	deploymentName := "deployment-" + string(uuid.NewUUID())
	deploymentSpec := &apps.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: namespace,
		},
		Spec: apps.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: podLabels,
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: podLabels,
				},
				Spec: v1.PodSpec{
					TerminationGracePeriodSeconds: &zero,
					Containers: []v1.Container{
						{
							Name:    "write-pod",
							Image:   "busybox",
							Command: []string{"/bin/sh"},
							Args:    []string{"-c", command},
							SecurityContext: &v1.SecurityContext{
								Privileged: &isPrivileged,
							},
						},
					},
					RestartPolicy: v1.RestartPolicyAlways,
				},
			},
		},
	}
	var volumeMounts = make([]v1.VolumeMount, len(pvclaims))
	var volumes = make([]v1.Volume, len(pvclaims))
	for index, _ := range pvclaims {
		volumename := fmt.Sprintf("flex-volume-%v", index)
		volumeMounts[index] = v1.VolumeMount{Name: volumename, MountPath: "/mnt/" + volumename}
		//		volumes[index] = v1.Volume{Name: volumename, VolumeSource: v1.VolumeSource{PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{ClaimName: pvclaim.Name, ReadOnly: false}}}
		volumes[index] = v1.Volume{Name: volumename, VolumeSource: v1.VolumeSource{FlexVolume: &v1.FlexVolumeSource{Driver: "k8s/dummy-attachable-" + namespace}}}
	}
	deploymentSpec.Spec.Template.Spec.Containers[0].VolumeMounts = volumeMounts
	deploymentSpec.Spec.Template.Spec.Volumes = volumes
	if nodeSelector != nil {
		deploymentSpec.Spec.Template.Spec.NodeSelector = nodeSelector
	}
	return deploymentSpec
}
