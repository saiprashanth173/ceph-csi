/*
Copyright 2021 The Ceph-CSI Authors.

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

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	snapapi "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2elog "k8s.io/kubernetes/test/e2e/framework/log"
)

const (
	adminUser = "admin"
)

// validateSubvolumegroup validates whether subvolumegroup is present.
func validateSubvolumegroup(f *framework.Framework, subvolgrp string) error {
	cmd := fmt.Sprintf("ceph fs subvolumegroup getpath %s %s", fileSystemName, subvolgrp)
	stdOut, stdErr, err := execCommandInToolBoxPod(f, cmd, rookNamespace)
	if err != nil {
		return fmt.Errorf("failed to exec command in toolbox: %w", err)
	}
	if stdErr != "" {
		return fmt.Errorf("failed to getpath for subvolumegroup %s : %v", subvolgrp, stdErr)
	}
	expectedGrpPath := "/volumes/" + subvolgrp
	stdOut = strings.TrimSpace(stdOut)
	if stdOut != expectedGrpPath {
		return fmt.Errorf("error unexpected group path. Found: %s", stdOut)
	}

	return nil
}

func createCephfsStorageClass(
	c kubernetes.Interface,
	f *framework.Framework,
	enablePool bool,
	params map[string]string,
) error {
	scPath := fmt.Sprintf("%s/%s", cephFSExamplePath, "storageclass.yaml")
	sc, err := getStorageClass(scPath)
	if err != nil {
		return err
	}
	sc.Parameters["fsName"] = fileSystemName
	sc.Parameters["csi.storage.k8s.io/provisioner-secret-namespace"] = cephCSINamespace
	sc.Parameters["csi.storage.k8s.io/provisioner-secret-name"] = cephFSProvisionerSecretName

	sc.Parameters["csi.storage.k8s.io/controller-expand-secret-namespace"] = cephCSINamespace
	sc.Parameters["csi.storage.k8s.io/controller-expand-secret-name"] = cephFSProvisionerSecretName

	sc.Parameters["csi.storage.k8s.io/node-stage-secret-namespace"] = cephCSINamespace
	sc.Parameters["csi.storage.k8s.io/node-stage-secret-name"] = cephFSNodePluginSecretName

	if enablePool {
		sc.Parameters["pool"] = "myfs-replicated"
	}

	// overload any parameters that were passed
	if params == nil {
		// create an empty params, so that params["clusterID"] below
		// does not panic
		params = map[string]string{}
	}
	for param, value := range params {
		sc.Parameters[param] = value
	}

	// fetch and set fsID from the cluster if not set in params
	if _, found := params["clusterID"]; !found {
		var fsID string
		fsID, err = getClusterID(f)
		if err != nil {
			return fmt.Errorf("failed to get clusterID: %w", err)
		}
		sc.Parameters["clusterID"] = fsID
	}

	timeout := time.Duration(deployTimeout) * time.Minute

	return wait.PollImmediate(poll, timeout, func() (bool, error) {
		_, err = c.StorageV1().StorageClasses().Create(context.TODO(), &sc, metav1.CreateOptions{})
		if err != nil {
			e2elog.Logf("error creating StorageClass %q: %v", sc.Name, err)
			if isRetryableAPIError(err) {
				return false, nil
			}

			return false, fmt.Errorf("failed to create StorageClass %q: %w", sc.Name, err)
		}

		return true, nil
	})
}

func createCephfsSecret(f *framework.Framework, secretName, userName, userKey string) error {
	scPath := fmt.Sprintf("%s/%s", cephFSExamplePath, "secret.yaml")
	sc, err := getSecret(scPath)
	if err != nil {
		return err
	}
	if secretName != "" {
		sc.Name = secretName
	}
	sc.StringData["adminID"] = userName
	sc.StringData["adminKey"] = userKey
	delete(sc.StringData, "userID")
	delete(sc.StringData, "userKey")
	sc.Namespace = cephCSINamespace
	_, err = f.ClientSet.CoreV1().Secrets(cephCSINamespace).Create(context.TODO(), &sc, metav1.CreateOptions{})

	return err
}

// unmountCephFSVolume unmounts a cephFS volume mounted on a pod.
func unmountCephFSVolume(f *framework.Framework, appName, pvcName string) error {
	pod, err := f.ClientSet.CoreV1().Pods(f.UniqueName).Get(context.TODO(), appName, metav1.GetOptions{})
	if err != nil {
		e2elog.Logf("Error occurred getting pod %s in namespace %s", appName, f.UniqueName)

		return fmt.Errorf("failed to get pod: %w", err)
	}
	pvc, err := getPersistentVolumeClaim(f.ClientSet, f.UniqueName, pvcName)
	if err != nil {
		e2elog.Logf("Error occurred getting PVC %s in namespace %s", pvcName, f.UniqueName)

		return fmt.Errorf("failed to get pvc: %w", err)
	}
	cmd := fmt.Sprintf(
		"umount /var/lib/kubelet/pods/%s/volumes/kubernetes.io~csi/%s/mount",
		pod.UID,
		pvc.Spec.VolumeName)
	stdErr, err := execCommandInDaemonsetPod(
		f,
		cmd,
		cephFSDeamonSetName,
		pod.Spec.NodeName,
		cephFSContainerName,
		cephCSINamespace)
	if stdErr != "" {
		e2elog.Logf("StdErr occurred: %s", stdErr)
	}

	return err
}

func deleteBackingCephFSVolume(f *framework.Framework, pvc *v1.PersistentVolumeClaim) error {
	imageData, err := getImageInfoFromPVC(pvc.Namespace, pvc.Name, f)
	if err != nil {
		return err
	}

	cmd := fmt.Sprintf("ceph fs subvolume rm %s %s %s", fileSystemName, imageData.imageName, subvolumegroup)
	_, stdErr, err := execCommandInToolBoxPod(f, cmd, rookNamespace)
	if err != nil {
		return err
	}
	if stdErr != "" {
		return fmt.Errorf("error deleting backing volume %s %v", imageData.imageName, stdErr)
	}

	return nil
}

type cephfsSubVolume struct {
	Name string `json:"name"`
}

func listCephFSSubVolumes(f *framework.Framework, filesystem, groupname string) ([]cephfsSubVolume, error) {
	var subVols []cephfsSubVolume
	stdout, stdErr, err := execCommandInToolBoxPod(
		f,
		fmt.Sprintf("ceph fs subvolume ls %s --group_name=%s --format=json", filesystem, groupname),
		rookNamespace)
	if err != nil {
		return subVols, err
	}
	if stdErr != "" {
		return subVols, fmt.Errorf("error listing subvolumes %v", stdErr)
	}

	err = json.Unmarshal([]byte(stdout), &subVols)
	if err != nil {
		return subVols, err
	}

	return subVols, nil
}

type cephfsSubvolumeMetadata struct {
	PVCNameKey      string `json:"csi.storage.k8s.io/pvc/name"`
	PVCNamespaceKey string `json:"csi.storage.k8s.io/pvc/namespace"`
	PVNameKey       string `json:"csi.storage.k8s.io/pv/name"`
	ClusterNameKey  string `json:"csi.ceph.com/cluster/name"`
}

func listCephFSSubvolumeMetadata(
	f *framework.Framework,
	filesystem,
	subvolume,
	groupname string,
) (*cephfsSubvolumeMetadata, error) {
	stdout, stdErr, err := execCommandInToolBoxPod(
		f,
		fmt.Sprintf("ceph fs subvolume metadata ls %s %s --group_name=%s --format=json", filesystem, subvolume, groupname),
		rookNamespace)
	if err != nil {
		return nil, err
	}
	if stdErr != "" {
		return nil, fmt.Errorf("error listing subvolume metadata %v", stdErr)
	}

	metadata := &cephfsSubvolumeMetadata{}
	err = json.Unmarshal([]byte(stdout), metadata)
	if err != nil {
		return metadata, err
	}

	return metadata, nil
}

type cephfsSnapshotMetadata struct {
	VolSnapNameKey        string `json:"csi.storage.k8s.io/volumesnapshot/name"`
	VolSnapNamespaceKey   string `json:"csi.storage.k8s.io/volumesnapshot/namespace"`
	VolSnapContentNameKey string `json:"csi.storage.k8s.io/volumesnapshotcontent/name"`
	ClusterNameKey        string `json:"csi.ceph.com/cluster/name"`
}

func listCephFSSnapshotMetadata(
	f *framework.Framework,
	filesystem,
	subvolume,
	snapname,
	groupname string,
) (*cephfsSnapshotMetadata, error) {
	stdout, stdErr, err := execCommandInToolBoxPod(
		f,
		fmt.Sprintf("ceph fs subvolume snapshot metadata ls %s %s %s --group_name=%s --format=json",
			filesystem, subvolume, snapname, groupname),
		rookNamespace)
	if err != nil {
		return nil, err
	}
	if stdErr != "" {
		return nil, fmt.Errorf("error listing subvolume snapshots metadata %v", stdErr)
	}

	metadata := &cephfsSnapshotMetadata{}
	err = json.Unmarshal([]byte(stdout), metadata)
	if err != nil {
		return metadata, err
	}

	return metadata, nil
}

type cephfsSnapshot struct {
	Name string `json:"name"`
}

func listCephFSSnapshots(f *framework.Framework, filesystem, subvolume, groupname string) ([]cephfsSnapshot, error) {
	var snaps []cephfsSnapshot
	stdout, stdErr, err := execCommandInToolBoxPod(
		f,
		fmt.Sprintf("ceph fs subvolume snapshot ls %s %s --group_name=%s --format=json", filesystem, subvolume, groupname),
		rookNamespace)
	if err != nil {
		return snaps, err
	}
	if stdErr != "" {
		return snaps, fmt.Errorf("error listing subolume snapshots %v", stdErr)
	}

	err = json.Unmarshal([]byte(stdout), &snaps)
	if err != nil {
		return snaps, err
	}

	return snaps, nil
}

// getSubvolumepath validates whether subvolumegroup is present.
func getSubvolumePath(f *framework.Framework, filesystem, subvolgrp, subvolume string) (string, error) {
	cmd := fmt.Sprintf("ceph fs subvolume getpath %s %s --group_name=%s", filesystem, subvolume, subvolgrp)
	stdOut, stdErr, err := execCommandInToolBoxPod(f, cmd, rookNamespace)
	if err != nil {
		return "", err
	}
	if stdErr != "" {
		return "", fmt.Errorf("failed to getpath for subvolume %s : %s", subvolume, stdErr)
	}

	return strings.TrimSpace(stdOut), nil
}

func getSnapName(snapNamespace, snapName string) (string, error) {
	sclient, err := newSnapshotClient()
	if err != nil {
		return "", err
	}
	snap, err := sclient.
		VolumeSnapshots(snapNamespace).
		Get(context.TODO(), snapName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get volumesnapshot: %w", err)
	}
	sc, err := sclient.
		VolumeSnapshotContents().
		Get(context.TODO(), *snap.Status.BoundVolumeSnapshotContentName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get volumesnapshotcontent: %w", err)
	}
	snapIDRegex := regexp.MustCompile(`(\w+\-?){5}$`)
	snapID := snapIDRegex.FindString(*sc.Status.SnapshotHandle)
	snapshotName := fmt.Sprintf("csi-snap-%s", snapID)
	e2elog.Logf("snapshotName= %s", snapshotName)

	return snapshotName, nil
}

func deleteBackingCephFSSubvolumeSnapshot(
	f *framework.Framework,
	pvc *v1.PersistentVolumeClaim,
	snap *snapapi.VolumeSnapshot,
) error {
	snapshotName, err := getSnapName(snap.Namespace, snap.Name)
	if err != nil {
		return err
	}
	imageData, err := getImageInfoFromPVC(pvc.Namespace, pvc.Name, f)
	if err != nil {
		return err
	}
	cmd := fmt.Sprintf(
		"ceph fs subvolume snapshot rm %s %s %s %s",
		fileSystemName,
		imageData.imageName,
		snapshotName,
		subvolumegroup)
	_, stdErr, err := execCommandInToolBoxPod(f, cmd, rookNamespace)
	if err != nil {
		return err
	}
	if stdErr != "" {
		return fmt.Errorf("error deleting backing snapshot %s %v", snapshotName, stdErr)
	}

	return nil
}
