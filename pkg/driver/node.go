/*
Copyright © 2019 The OpenEBS Authors

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

package driver

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-iscsi/iscsi"
	"github.com/openebs/jiva-csi/pkg/kubernetes/client"
	"github.com/openebs/jiva-csi/pkg/request"
	"github.com/openebs/jiva-csi/pkg/utils"
	jv "github.com/openebs/jiva-operator/pkg/apis/openebs/v1alpha1"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/errors"
)

const (
	// FSTypeExt2 represents the ext2 filesystem type
	FSTypeExt2 = "ext2"
	// FSTypeExt3 represents the ext3 filesystem type
	FSTypeExt3 = "ext3"
	// FSTypeExt4 represents the ext4 filesystem type
	FSTypeExt4 = "ext4"
	// FSTypeXfs represents te xfs filesystem type
	FSTypeXfs = "xfs"

	defaultFsType = FSTypeExt4

	defaultISCSILUN       = int32(0)
	defaultISCSIInterface = "default"
)

var (
	// ValidFSTypes is the supported filesystem by the jiva-csi driver
	ValidFSTypes = []string{FSTypeExt2, FSTypeExt3, FSTypeExt4, FSTypeXfs}
	// MaxRetryCount is the retry count to check if volume is ready during
	// nodeStage RPC call
	MaxRetryCount int
)

var (
	// nodeCaps represents the capability of node service.
	nodeCaps = []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
	}
)

type nodeStageRequest struct {
	stagingPath string
	fsType      string
	volumeID    string
}

// node is the server implementation
// for CSI NodeServer
type node struct {
	client           *client.Client
	driver           *CSIDriver
	mounter          *NodeMounter
	volumeTransition *request.Transition
}

// NewNode returns a new instance
// of CSI NodeServer
func NewNode(d *CSIDriver, cli *client.Client) csi.NodeServer {
	return &node{
		client:           cli,
		driver:           d,
		mounter:          newNodeMounter(),
		volumeTransition: request.NewTransition(),
	}
}

func (ns *node) attachDisk(instance *jv.JivaVolume) (string, error) {
	connector := iscsi.Connector{
		VolumeName:    instance.Name,
		TargetIqn:     instance.Spec.ISCSISpec.Iqn,
		Lun:           defaultISCSILUN,
		Interface:     defaultISCSIInterface,
		TargetPortals: []string{fmt.Sprintf("%v:%v", instance.Spec.ISCSISpec.TargetIP, instance.Spec.ISCSISpec.TargetPort)},
		DoDiscovery:   true,
	}

	logrus.Debugf("NodeStageVolume: attach disk with config: {%+v}", connector)
	devicePath, err := iscsi.Connect(connector)
	if err != nil {
		return "", err
	}

	if devicePath == "" {
		return "", fmt.Errorf("connect reported success, but no path returned")
	}
	return devicePath, err
}

func (ns *node) waitForVolumeToBeReady(volID string) (*jv.JivaVolume, error) {
	var retry int
	var sleepInterval time.Duration = 0
	for {
		time.Sleep(sleepInterval * time.Second)
		instance, err := ns.doesVolumeExist(volID)
		if err != nil {
			return nil, err
		}

		retry++
		if instance.Status.Phase == jv.JivaVolumePhaseReady && instance.Status.Status == "RW" {
			return instance, nil
		} else if retry <= MaxRetryCount {
			sleepInterval = 5
			if instance.Status.Status == "RO" {
				replicaStatus := instance.Status.ReplicaStatuses
				if len(replicaStatus) != 0 {
					logrus.Warningf("Volume is in RO mode: replica status: {%+v}", replicaStatus)
					continue
				}
				logrus.Warningf("Volume is not ready: replicas may not be connected")
				continue
			}
			logrus.Warningf("Volume is not ready: volume status is %s", instance.Status.Status)
			continue
		} else {
			break
		}
	}
	return nil, fmt.Errorf("Max retry count exceeded, volume is not ready")
}

func (ns *node) waitForVolumeToBeReachable(targetPortal string) error {
	var (
		retries int
		err     error
		conn    net.Conn
	)

	for {
		// Create a connection to test if the iSCSI Portal is reachable,
		if conn, err = net.Dial("tcp", targetPortal); err == nil {
			conn.Close()
			logrus.Debugf("Target {%v} is reachable to create connections", targetPortal)
			return nil
		}
		// wait until the iSCSI targetPortal is reachable
		// There is no pointn of triggering iSCSIadm login commands
		// until the portal is reachable
		time.Sleep(2 * time.Second)
		retries++
		if retries >= MaxRetryCount {
			// Let the caller function decide further if the volume is
			// not reachable even after 12 seconds ( This number was arrived at
			// based on the kubelets retrying logic. Kubelet retries to publish
			// volume after every 14s )
			return fmt.Errorf(
				"iSCSI Target not reachable, TargetPortal {%v}, err:%v",
				targetPortal, err)
		}
	}
}

func (ns *node) validateStagingReq(req *csi.NodeStageVolumeRequest) (nodeStageRequest, error) {
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nodeStageRequest{}, status.Error(codes.InvalidArgument, "Volume ID not provided")
	}

	volID := utils.StripName(volumeID)
	volCap := req.GetVolumeCapability()
	if volCap == nil {
		return nodeStageRequest{}, status.Error(codes.InvalidArgument, "Volume capability not provided")
	}

	if !isValidVolumeCapabilities([]*csi.VolumeCapability{volCap}) {
		return nodeStageRequest{}, status.Error(codes.InvalidArgument, "Volume capability not supported")
	}

	mount := volCap.GetMount()
	if mount == nil {
		return nodeStageRequest{}, status.Error(codes.InvalidArgument, "NodeStageVolume: mount is nil within volume capability")
	}

	fsType := mount.GetFsType()
	if len(fsType) == 0 {
		fsType = defaultFsType
	}

	stagingPath := req.GetStagingTargetPath()
	if len(stagingPath) == 0 {
		return nodeStageRequest{}, status.Error(codes.InvalidArgument, "staging path is empty")
	}

	return nodeStageRequest{
		volumeID:    volID,
		fsType:      fsType,
		stagingPath: stagingPath,
	}, nil
}

// NodeStageVolume mounts the volume on the staging
// path
//
// This implements csi.NodeServer
func (ns *node) NodeStageVolume(
	ctx context.Context,
	req *csi.NodeStageVolumeRequest,
) (*csi.NodeStageVolumeResponse, error) {

	reqParam, err := ns.validateStagingReq(req)
	if err != nil {
		return nil, err
	}
	logrus.Infof("NodeStageVolume: start volume: {%q} operation", reqParam.volumeID)
	if ok := ns.volumeTransition.Insert(reqParam.volumeID); !ok {
		msg := fmt.Sprintf("an operation on this volume=%q is already in progress", reqParam.volumeID)
		return nil, status.Error(codes.Aborted, msg)
	}
	defer func() {
		logrus.Infof("NodeStageVolume: volume: {%q} operation finished", reqParam.volumeID)
		ns.volumeTransition.Delete(reqParam.volumeID)
	}()

	// Check if volume is ready to serve IOs,
	// info is fetched from the JivaVolume CR
	logrus.Debug("NodeStageVolume: wait for the volume to be ready")
	instance, err := ns.waitForVolumeToBeReady(reqParam.volumeID)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}

	// Volume may be mounted at targetPath (bind mount in NodePublish)
	if err := ns.isAlreadyMounted(reqParam.volumeID, reqParam.stagingPath); err != nil {
		return nil, err
	}

	// A temporary TCP connection is made to the volume to check if its
	// reachable
	logrus.Debug("NodeStageVolume: wait for the iscsi target to be ready")
	if err := ns.waitForVolumeToBeReachable(
		fmt.Sprintf("%v:%v", instance.Spec.ISCSISpec.TargetIP,
			instance.Spec.ISCSISpec.TargetPort),
	); err != nil {
		return nil,
			status.Error(codes.FailedPrecondition, err.Error())
	}

	devicePath, err := ns.attachDisk(instance)
	if err != nil {
		logrus.Errorf("NodeStageVolume: failed to attachDisk for volume %v, err: %v", reqParam.volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	// JivaVolume CR may be updated by jiva-operator
	instance, err = ns.client.GetJivaVolume(reqParam.volumeID)
	if err != nil {
		return nil, err
	}

	instance.Spec.MountInfo.FSType = reqParam.fsType
	instance.Spec.MountInfo.DevicePath = devicePath
	instance.Spec.MountInfo.Path = reqParam.stagingPath
	if err := ns.client.UpdateJivaVolume(instance); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if err := os.MkdirAll(reqParam.stagingPath, 0750); err != nil {
		logrus.Errorf("failed to mkdir %s, error: %v", reqParam.stagingPath, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	logrus.Info("NodeStageVolume: start format and mount operation")
	if err := ns.formatAndMount(req, instance.Spec.MountInfo.DevicePath); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *node) doesVolumeExist(volID string) (*jv.JivaVolume, error) {
	volID = utils.StripName(volID)
	if err := ns.client.Set(); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	instance, err := ns.client.GetJivaVolume(volID)
	if err != nil && errors.IsNotFound(err) {
		return nil, status.Error(codes.NotFound, err.Error())
	} else if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return instance, nil
}

// NodeUnstageVolume unmounts the volume from
// the staging path
//
// This implements csi.NodeServer
func (ns *node) NodeUnstageVolume(
	ctx context.Context,
	req *csi.NodeUnstageVolumeRequest,
) (*csi.NodeUnstageVolumeResponse, error) {

	volID := req.GetVolumeId()
	if volID == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeUnstageVolume Volume ID must be provided")
	}

	target := req.GetStagingTargetPath()
	if len(target) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Staging target not provided")
	}

	if ok := ns.volumeTransition.Insert(volID); !ok {
		msg := fmt.Sprintf("an operation on this volume=%q is already in progress", volID)
		return nil, status.Error(codes.Aborted, msg)
	}

	defer func() {
		logrus.Infof("NodeUnstageVolume: volume: {%q} operation finished", volID)
		ns.volumeTransition.Delete(volID)
	}()

	// Check if target directory is a mount point. GetDeviceNameFromMount
	// given a mnt point, finds the device from /proc/mounts
	// returns the device name, reference count, and error code
	dev, refCount, err := ns.mounter.GetDeviceName(target)
	if err != nil {
		msg := fmt.Sprintf("failed to check if volume is mounted: %v", err)
		return nil, status.Error(codes.Internal, msg)
	}

	// From the spec: If the volume corresponding to the volume_id
	// is not staged to the staging_target_path, the Plugin MUST
	// reply 0 OK.
	if refCount == 0 {
		logrus.Infof("NodeUnstageVolume: %s target not mounted", target)
		return &csi.NodeUnstageVolumeResponse{}, nil
	}

	if refCount > 1 {
		logrus.Warningf("NodeUnstageVolume: found %d references to device %s mounted at target path %s", refCount, dev, target)
	}

	logrus.Debugf("NodeUnstageVolume: unmounting %s", target)
	err = ns.mounter.Unmount(target)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not unmount target %q: %v", target, err)
	}

	instance, err := ns.doesVolumeExist(volID)
	if err != nil {
		return nil, err
	}

	logrus.Infof("NodeUnstageVolume: disconnect from iscsi target: %s", target)
	if err := iscsi.Disconnect(instance.Spec.ISCSISpec.Iqn, []string{fmt.Sprintf("%v:%v",
		instance.Spec.ISCSISpec.TargetIP, instance.Spec.ISCSISpec.TargetPort)}); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if err := os.RemoveAll(instance.Spec.MountInfo.Path); err != nil {
		logrus.Errorf("Failed to remove mount path, err: %v", err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	logrus.Infof("NodeUnstageVolume: detaching device %v is finished", instance.Spec.MountInfo.DevicePath)

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *node) formatAndMount(req *csi.NodeStageVolumeRequest, devicePath string) error {
	// Mount device
	mntPath := req.GetStagingTargetPath()
	notMnt, err := ns.mounter.IsLikelyNotMountPoint(mntPath)
	if err != nil && !os.IsNotExist(err) {
		if err := os.MkdirAll(mntPath, 0750); err != nil {
			logrus.Errorf("failed to mkdir %s, error", mntPath)
			return err
		}
	}

	if !notMnt {
		logrus.Infof("Volume %s has been mounted already at %v", req.GetVolumeId(), mntPath)
		return nil
	}

	fsType := req.GetVolumeCapability().GetMount().GetFsType()
	options := []string{}
	mountFlags := req.GetVolumeCapability().GetMount().GetMountFlags()
	options = append(options, mountFlags...)

	err = ns.mounter.FormatAndMount(devicePath, mntPath, fsType, options)
	if err != nil {
		logrus.Errorf(
			"Failed to mount iscsi volume %s [%s, %s] to %s, error %v",
			req.GetVolumeId(), devicePath, fsType, mntPath, err,
		)
		return err
	}
	return nil
}

// NodePublishVolume publishes (mounts) the volume
// at the corresponding node at a given path
//
// This implements csi.NodeServer
func (ns *node) NodePublishVolume(
	ctx context.Context,
	req *csi.NodePublishVolumeRequest,
) (*csi.NodePublishVolumeResponse, error) {

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
	}

	target := req.GetTargetPath()
	if len(target) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path not provided")
	}

	volCap := req.GetVolumeCapability()
	if volCap == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability not provided")
	}

	if !isValidVolumeCapabilities([]*csi.VolumeCapability{volCap}) {
		return nil, status.Error(codes.InvalidArgument, "Volume capability not supported")
	}

	logrus.Infof("NodePublishVolume: start volume: {%q} operation", volumeID)
	if ok := ns.volumeTransition.Insert(volumeID); !ok {
		msg := fmt.Sprintf("an operation on this volume=%q is already in progress", volumeID)
		return nil, status.Error(codes.Aborted, msg)
	}

	defer func() {
		logrus.Infof("NodePublishVolume: volume: {%q} operation finished", volumeID)
		ns.volumeTransition.Delete(volumeID)
	}()

	// Volume may be mounted at targetPath (bind mount in NodePublish)
	if err := ns.isAlreadyMounted(volumeID, target); err != nil {
		return nil, err
	}

	mountOptions := []string{"bind"}
	if req.GetReadonly() {
		mountOptions = append(mountOptions, "ro")
	}
	switch mode := volCap.GetAccessType().(type) {
	case *csi.VolumeCapability_Block:
		return &csi.NodePublishVolumeResponse{}, status.Error(codes.Unimplemented, "doesn't support block device provisioning")
	case *csi.VolumeCapability_Mount:
		if err := ns.nodePublishVolumeForFileSystem(req, mountOptions, mode); err != nil {
			return nil, err
		}
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *node) nodePublishVolumeForFileSystem(req *csi.NodePublishVolumeRequest, mountOptions []string, mode *csi.VolumeCapability_Mount) error {
	target := req.GetTargetPath()
	source := req.GetStagingTargetPath()
	if m := mode.Mount; m != nil {
		hasOption := func(options []string, opt string) bool {
			for _, o := range options {
				if o == opt {
					return true
				}
			}
			return false
		}
		for _, f := range m.MountFlags {
			if !hasOption(mountOptions, f) {
				mountOptions = append(mountOptions, f)
			}
		}
	}

	logrus.Infof("NodePublishVolume: creating dir %s", target)
	if err := os.MkdirAll(target, 0000); err != nil {
		return status.Errorf(codes.Internal, "Could not create dir {%q}, err: %v", target, err)
	}

	// in case if the dir already exists, above call returns nil
	// so permission needs to be updated
	if err := os.Chmod(target, 0000); err != nil {
		return status.Errorf(codes.Internal, "Could not change mode of dir {%q}, err: %v", target, err)
	}

	fsType := mode.Mount.GetFsType()
	if len(fsType) == 0 {
		fsType = defaultFsType
	}

	logrus.Infof("NodePublishVolume: mounting %s at %s with option %s as fstype %s", source, target, mountOptions, fsType)
	if err := ns.mounter.Mount(source, target, fsType, mountOptions); err != nil {
		if removeErr := os.Remove(target); removeErr != nil {
			return status.Errorf(codes.Internal, "Could not remove mount target %q: %v", target, err)
		}
		return status.Errorf(codes.Internal, "Could not mount %q at %q: %v", source, target, err)
	}

	return nil
}

// NodeUnpublishVolume unpublishes (unmounts) the volume
// from the corresponding node from the given path
//
// This implements csi.NodeServer
func (ns *node) NodeUnpublishVolume(
	ctx context.Context,
	req *csi.NodeUnpublishVolumeRequest,
) (*csi.NodeUnpublishVolumeResponse, error) {

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
	}

	target := req.GetTargetPath()
	if len(target) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path not provided")
	}

	if ok := ns.volumeTransition.Insert(volumeID); !ok {
		msg := fmt.Sprintf("an operation on this volume=%q is already in progress", volumeID)
		return nil, status.Error(codes.Aborted, msg)
	}

	defer func() {
		logrus.Infof("NodeUnPublishVolume: volume: {%q} operation finished", volumeID)
		ns.volumeTransition.Delete(volumeID)
	}()

	if err := ns.unmount(volumeID, target); err != nil {
		return nil, err
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *node) isAlreadyMounted(volID, path string) error {
	currentMounts := map[string]bool{}
	mountList, err := ns.mounter.List()
	if err != nil {
		return fmt.Errorf("Failed to list mount paths")
	}

	for _, mntInfo := range mountList {
		if strings.Contains(mntInfo.Path, volID) {
			currentMounts[mntInfo.Path] = true
		}
	}

	// if volume is mounted at more than one place check if this request is
	// for the same path that is already mounted. Return nil if the path is
	// mounted already else return err so that it gets unmounted in the
	// next subsequent calls in respective rpc calls (NodeUnpublishVolume, NodeUnstageVolume)
	if len(currentMounts) > 1 {
		if mounted, ok := currentMounts[path]; ok && mounted {
			return nil
		}
		return fmt.Errorf("Volume is already mounted at more than one place: {%v}", currentMounts)
	}

	return nil
}

func (ns *node) unmount(volumeID, target string) error {
	notMnt, err := ns.mounter.IsLikelyNotMountPoint(target)
	if (err == nil && notMnt) || os.IsNotExist(err) {
		logrus.Warningf("Volume: %s is not mounted, err: %v", target, err)
		return nil
	}

	logrus.Infof("Unmounting: %s", target)
	if err := ns.mounter.Unmount(target); err != nil {
		return status.Errorf(codes.Internal, "Could not unmount %q: %v", target, err)
	}
	return nil
}

// NodeGetInfo returns node details
//
// This implements csi.NodeServer
func (ns *node) NodeGetInfo(
	ctx context.Context,
	req *csi.NodeGetInfoRequest,
) (*csi.NodeGetInfoResponse, error) {

	return &csi.NodeGetInfoResponse{
		NodeId: ns.driver.config.NodeID,
	}, nil
}

// NodeGetCapabilities returns capabilities supported
// by this node service
//
// This implements csi.NodeServer
func (ns *node) NodeGetCapabilities(
	ctx context.Context,
	req *csi.NodeGetCapabilitiesRequest,
) (*csi.NodeGetCapabilitiesResponse, error) {

	var caps []*csi.NodeServiceCapability
	for _, cap := range nodeCaps {
		c := &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: cap,
				},
			},
		}
		caps = append(caps, c)
	}
	return &csi.NodeGetCapabilitiesResponse{Capabilities: caps}, nil
}

// TODO
// Verify if this needs to be implemented
//
// NodeExpandVolume resizes the filesystem if required
//
// If ControllerExpandVolumeResponse returns true in
// node_expansion_required then FileSystemResizePending
// condition will be added to PVC and NodeExpandVolume
// operation will be queued on kubelet
//
// This implements csi.NodeServer
func (ns *node) NodeExpandVolume(
	ctx context.Context,
	req *csi.NodeExpandVolumeRequest,
) (*csi.NodeExpandVolumeResponse, error) {

	return nil, nil
}

// NodeGetVolumeStats returns statistics for the
// given volume
//
// This implements csi.NodeServer
func (ns *node) NodeGetVolumeStats(
	ctx context.Context,
	req *csi.NodeGetVolumeStatsRequest,
) (*csi.NodeGetVolumeStatsResponse, error) {

	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeGetVolumeStats Volume ID must be provided")
	}

	volumePath := req.GetVolumePath()
	if volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeGetVolumeStats Volume Path must be provided")
	}

	mounted, err := ns.mounter.ExistsPath(volumePath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to check if volume path %q is mounted: %s", volumePath, err)
	}

	if !mounted {
		return nil, status.Errorf(codes.NotFound, "volume path %q is not mounted", volumePath)
	}

	stats, err := getStatistics(volumePath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to retrieve capacity statistics for volume path %q: %s", volumePath, err)
	}

	return &csi.NodeGetVolumeStatsResponse{
		Usage: stats,
	}, nil
}

func (ns *node) validateNodePublishReq(
	req *csi.NodePublishVolumeRequest,
) error {
	if req.GetVolumeCapability() == nil {
		return status.Error(codes.InvalidArgument,
			"Volume capability missing in request")
	}

	if len(req.GetVolumeId()) == 0 {
		return status.Error(codes.InvalidArgument,
			"Volume ID missing in request")
	}
	return nil
}

func (ns *node) validateNodeUnpublishReq(
	req *csi.NodeUnpublishVolumeRequest,
) error {
	if req.GetVolumeId() == "" {
		return status.Error(codes.InvalidArgument,
			"Volume ID missing in request")
	}

	if req.GetTargetPath() == "" {
		return status.Error(codes.InvalidArgument,
			"Target path missing in request")
	}
	return nil
}
