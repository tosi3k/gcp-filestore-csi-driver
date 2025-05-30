/*
Copyright 2022 The Kubernetes Authors.

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
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	filev1beta1multishare "google.golang.org/api/file/v1beta1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
	cloud "sigs.k8s.io/gcp-filestore-csi-driver/pkg/cloud_provider"
	"sigs.k8s.io/gcp-filestore-csi-driver/pkg/cloud_provider/file"
	"sigs.k8s.io/gcp-filestore-csi-driver/pkg/util"
)

type OpInfo struct {
	Id     string
	Type   util.OperationType
	Target string
}

// A workflow is defined as a sequence of steps to safely initiate instance or share operations.
type Workflow struct {
	instance *file.MultishareInstance
	share    *file.Share
	opType   util.OperationType
	opName   string
}

// MultishareOpsManager manages the lifecycle of all instance and share operations.
type MultishareOpsManager struct {
	sync.Mutex         // Lock to perform thread safe multishare operations.
	cloud              *cloud.Cloud
	controllerServer   *controllerServer
	msControllerServer *MultishareController
}

func NewMultishareOpsManager(cloud *cloud.Cloud, mcs *MultishareController) *MultishareOpsManager {
	return &MultishareOpsManager{
		cloud:              cloud,
		msControllerServer: mcs,
	}
}

// setupEligibleInstanceAndStartWorkflow returns a workflow object (to indicate an instance or share level workflow is started), or a share object (if existing share already found), or error.
func (m *MultishareOpsManager) setupEligibleInstanceAndStartWorkflow(ctx context.Context, req *csi.CreateVolumeRequest, instance *file.MultishareInstance, sourceSnapshotId string) (*Workflow, *file.Share, error) {
	m.Lock()
	defer m.Unlock()

	// Check ShareCreateMap if a share create is already in progress.
	shareName := util.ConvertVolToShareName(req.Name)

	ops, err := m.listMultishareResourceRunningOps(ctx)
	if err != nil {
		return nil, nil, err
	}
	createShareOp := containsOpWithShareName(shareName, util.ShareCreate, ops)
	if createShareOp != nil {
		msg := fmt.Sprintf("Share create op %s in progress", createShareOp.Id)
		klog.Info(msg)
		return nil, nil, status.Error(codes.Aborted, msg)
	}

	// Check if share already part of an existing instance.
	regions, err := m.listRegions(req.GetAccessibilityRequirements())
	if err != nil {
		return nil, nil, status.Error(codes.InvalidArgument, err.Error())
	}
	for _, region := range regions {
		shares, err := m.cloud.File.ListShares(ctx, &file.ListFilter{Project: m.cloud.Project, Location: region, InstanceName: "-"})

		if err != nil {
			return nil, nil, err
		}
		for _, s := range shares {
			if s.Name == shareName && s.Parent.Protocol == instance.Protocol {
				return nil, s, nil
			}
		}
	}

	// No share or running share create op found. Proceed to eligible instance check.
	eligible, err := m.runEligibleInstanceCheck(ctx, req, ops, instance, regions)
	if err != nil {
		return nil, nil, status.Error(codes.Aborted, err.Error())
	}

	if len(eligible) > 0 {
		// pick a random eligible instance
		index := rand.Intn(len(eligible))
		klog.V(5).Infof("For share %s, using instance %s as placeholder", shareName, eligible[index].String())
		share, err := generateNewShare(shareName, eligible[index], req, sourceSnapshotId)
		if err != nil {
			return nil, nil, status.Error(codes.Internal, err.Error())
		}

		needExpand, targetBytes, err := m.instanceNeedsExpand(ctx, share, share.CapacityBytes)
		if err != nil {
			return nil, nil, err
		}

		if needExpand {
			eligible[index].CapacityBytes = targetBytes
			w, err := m.startInstanceWorkflow(ctx, &Workflow{instance: eligible[index], opType: util.InstanceUpdate}, ops)
			return w, nil, err
		}

		w, err := m.startShareWorkflow(ctx, &Workflow{share: share, opType: util.ShareCreate}, ops)
		return w, nil, err
	}

	param := req.GetParameters()
	// If we are creating a new instance, we need pick an unused CIDR range from reserved-ipv4-cidr
	// If the param was not provided, we default reservedIPRange to "" and cloud provider takes care of the allocation
	if instance.Network.ConnectMode == privateServiceAccess {
		if reservedIPRange, ok := param[ParamReservedIPRange]; ok {
			if IsCIDR(reservedIPRange) {
				return nil, nil, status.Error(codes.InvalidArgument, "When using connect mode PRIVATE_SERVICE_ACCESS, if reserved IP range is specified, it must be a named address range instead of direct CIDR value")
			}
			instance.Network.ReservedIpRange = reservedIPRange
		}
	} else if reservedIPV4CIDR, ok := param[ParamReservedIPV4CIDR]; ok {
		reservedIPRange, err := m.controllerServer.reserveIPRange(ctx, &file.ServiceInstance{
			Project:  instance.Project,
			Name:     instance.Name,
			Location: instance.Location,
			Tier:     instance.Tier,
			Network:  instance.Network,
		}, reservedIPV4CIDR)

		// Possible cases are 1) CreateInstanceAborted, 2)CreateInstance running in background
		// The ListInstances response will contain the reservedIPRange if the operation was started
		// In case of abort, the CIDR IP is released and available for reservation
		defer m.controllerServer.config.ipAllocator.ReleaseIPRange(reservedIPRange)
		if err != nil {
			return nil, nil, err
		}

		// Adding the reserved IP range to the instance object
		instance.Network.ReservedIpRange = reservedIPRange
	}

	w, err := m.startInstanceWorkflow(ctx, &Workflow{instance: instance, opType: util.InstanceCreate}, ops)
	return w, nil, err
}

func (m *MultishareOpsManager) listRegions(top *csi.TopologyRequirement) ([]string, error) {
	var allowedRegions []string
	clusterRegion, err := util.GetRegionFromZone(m.cloud.Zone)
	if err != nil {
		return allowedRegions, err
	}
	if top == nil {
		return append(allowedRegions, clusterRegion), nil
	}

	zones, err := listZonesFromTopology(top)
	if err != nil {
		return allowedRegions, err
	}

	seen := make(map[string]bool)
	for _, zone := range zones {
		region, err := util.GetRegionFromZone(zone)
		if err != nil {
			return allowedRegions, err
		}
		if !seen[region] {
			seen[region] = true
			allowedRegions = append(allowedRegions, region)
		}
	}

	if len(allowedRegions) == 0 {
		return append(allowedRegions, clusterRegion), nil
	}

	return allowedRegions, nil
}

func (m *MultishareOpsManager) startShareCreateWorkflowSafe(ctx context.Context, share *file.Share) (*Workflow, error) {
	m.Lock()
	defer m.Unlock()
	ops, err := m.listMultishareResourceRunningOps(ctx)
	if err != nil {
		return nil, err
	}

	return m.startShareWorkflow(ctx, &Workflow{share: share, opType: util.ShareCreate}, ops)
}

func (m *MultishareOpsManager) startInstanceWorkflow(ctx context.Context, w *Workflow, ops []*OpInfo) (*Workflow, error) {
	// This function has 2 steps:
	// 1. verify no instance ops or share (belonging to the instance) ops running for the given instance.
	// 2. Start the instance op.
	if w.instance == nil {
		return nil, status.Errorf(codes.Internal, "instance not found in workflow object")
	}

	err := m.verifyNoRunningInstanceOrShareOpsForInstance(w.instance, ops)
	if err != nil {
		return nil, err
	}
	switch w.opType {
	case util.InstanceCreate:
		op, err := m.cloud.File.StartCreateMultishareInstanceOp(ctx, w.instance)
		if err != nil {
			return nil, err
		}
		w.opName = op.Name
	case util.InstanceUpdate:
		op, err := m.cloud.File.StartResizeMultishareInstanceOp(ctx, w.instance)
		if err != nil {
			return nil, err
		}
		w.opName = op.Name
	case util.InstanceDelete:
		op, err := m.cloud.File.StartDeleteMultishareInstanceOp(ctx, w.instance)
		if err != nil {
			return nil, err
		}
		w.opName = op.Name
	default:
		return nil, status.Errorf(codes.Internal, "for instance workflow, unknown op type %s", w.opType.String())
	}

	return w, nil
}

func (m *MultishareOpsManager) verifyNoRunningInstanceOps(instance *file.MultishareInstance, ops []*OpInfo) error {
	instanceUri, err := file.GenerateMultishareInstanceURI(instance)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to parse instance handle, err: %v", err)
	}

	for _, op := range ops {
		if op.Target == instanceUri {
			return status.Errorf(codes.Aborted, "Found running op %s type %s for target resource %s", op.Id, op.Type.String(), op.Target)
		}
	}

	return nil
}

func (m *MultishareOpsManager) verifyNoRunningShareOps(share *file.Share, ops []*OpInfo) error {
	shareUri, err := file.GenerateShareURI(share)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to parse share handle, err: %v", err)
	}
	for _, op := range ops {
		if op.Target == shareUri {
			return status.Errorf(codes.Aborted, "Found running op %s type %s for target resource %s", op.Id, op.Type.String(), op.Target)
		}
	}

	return nil
}

func (m *MultishareOpsManager) startShareWorkflow(ctx context.Context, w *Workflow, ops []*OpInfo) (*Workflow, error) {
	// This function has 3 distinct steps:
	// 1. verify no instance ops running for the instance hosting the given share.
	// 2. verify no running ops for the given share.
	// 3. Start the share op.
	if w.share == nil {
		return nil, status.Errorf(codes.Internal, "share not found in workflow object")
	}

	if w.share.Parent == nil {
		return nil, status.Errorf(codes.Internal, "share parent not found in workflow object")
	}

	// verify instance is ready.
	err := m.verifyNoRunningInstanceOps(w.share.Parent, ops)
	if err != nil {
		return nil, err
	}
	// Verify share is ready.
	err = m.verifyNoRunningShareOps(w.share, ops)
	if err != nil {
		return nil, err
	}
	switch w.opType {
	case util.ShareCreate:
		op, err := m.cloud.File.StartCreateShareOp(ctx, w.share)
		if err != nil {
			return nil, err
		}
		w.opName = op.Name
	case util.ShareUpdate:
		op, err := m.cloud.File.StartResizeShareOp(ctx, w.share)
		if err != nil {
			return nil, err
		}
		w.opName = op.Name
	case util.ShareDelete:
		op, err := m.cloud.File.StartDeleteShareOp(ctx, w.share)
		if err != nil {
			return nil, err
		}
		w.opName = op.Name
	default:
		return nil, status.Errorf(codes.Internal, "for share workflow, unknown op type %v", w.opType)
	}
	return w, nil
}

func (m *MultishareOpsManager) verifyNoRunningInstanceOrShareOpsForInstance(instance *file.MultishareInstance, ops []*OpInfo) error {
	instanceUri, err := file.GenerateMultishareInstanceURI(instance)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to parse instance handle, err: %v", err.Error())
	}

	// Check for instance prefix in op target.
	for _, op := range ops {
		if op.Target == instanceUri || strings.Contains(op.Target, instanceUri+"/") {
			return status.Errorf(codes.Aborted, "Found running op %s, type %s, for target resource %s", op.Id, op.Type.String(), op.Target)
		}
	}
	return nil
}

// runEligibleInstanceCheck returns a list of ready and non-ready instances.
func (m *MultishareOpsManager) runEligibleInstanceCheck(ctx context.Context, req *csi.CreateVolumeRequest, ops []*OpInfo, target *file.MultishareInstance, regions []string) ([]*file.MultishareInstance, error) {
	klog.Infof("ListMultishareInstances call initiated for request %+v.", req)
	instances, err := m.listMatchedInstances(ctx, req, target, regions)
	if err != nil {
		return nil, err
	}
	klog.Infof("ListMultishareInstances call returned successfully with %d instances for request %+v.", len(instances), req)
	// An instance is considered as eligible if and only if the state is 'READY', and there's no ops running against it.
	var readyEligibleInstances []*file.MultishareInstance
	// An instance is considered as non-ready if any of the following conditions are met:
	// 1. The instance state is "CREATING" or "REPAIRING".
	// 2. The instance state is 'READY', but running ops are found on it.
	var nonReadyEligibleInstances []*file.MultishareInstance

	for _, instance := range instances {
		klog.Infof("Found multishare instance %s/%s/%s with state %s and max share count %d", instance.Project, instance.Location, instance.Name, instance.State, instance.MaxShareCount)
		if instance.State == "CREATING" || instance.State == "REPAIRING" {
			klog.Infof("Instance %s/%s/%s with state %s is not ready", instance.Project, instance.Location, instance.Name, instance.State)
			nonReadyEligibleInstances = append(nonReadyEligibleInstances, instance)
			continue
		}
		if instance.State != "READY" {
			klog.Infof("Instance %s/%s/%s with state %s is not eligible", instance.Project, instance.Location, instance.Name, instance.State)
			continue
			// TODO: If we saw instance states other than "CREATING" and "READY", we may need to do some special handlding in the future.
		}

		op, err := containsOpWithInstanceTargetPrefix(instance, ops)
		if err != nil {
			klog.Errorf("failed to check eligibility of instance %s", instance.Name)
			return nil, err
		}

		if op == nil {
			shares, err := m.cloud.File.ListShares(ctx, &file.ListFilter{Project: instance.Project, Location: instance.Location, InstanceName: instance.Name})
			if err != nil {
				klog.Errorf("Failed to list shares of instance %s/%s/%s, err:%v", instance.Project, instance.Location, instance.Name, err.Error())
				return nil, err
			}

			// If we encounter a scenario where the configurable shares per Filestore instance feature is disabled, CSI driver will continue to place max 10 shares per instance, irrespective of the actual max shares the Filestore instance can support.
			// Alternately, if CSI max share features is enabled, but filestore disables the feature, the create volume may continue to fail beyond 10 shares per instance.
			maxShareCount := util.MaxSharesPerInstance
			if m.msControllerServer != nil && m.msControllerServer.featureMaxSharePerInstance {
				maxShareCount = instance.MaxShareCount
			}
			if len(shares) >= maxShareCount {
				continue
			}

			readyEligibleInstances = append(readyEligibleInstances, instance)
			klog.Infof("Adding instance %s to eligible list", instance.String())
			continue
		}

		klog.Infof("Instance %s/%s/%s with state %s is not ready with ongoing operation %s type %s", instance.Project, instance.Location, instance.Name, instance.State, op.Id, op.Type.String())
		nonReadyEligibleInstances = append(nonReadyEligibleInstances, instance)

		// TODO: If we see > 1 instances with 0 shares (these could be possibly leaked instances where the driver hit timeout during creation op was in progress), should we trigger delete op for such instances? Possibly yes. Given that instance create/delete and share create/delete is serialized, maybe yes.
	}

	if len(readyEligibleInstances) == 0 && len(nonReadyEligibleInstances) > 0 {
		errorString := "All eligible filestore instances are busy.\n"

		for _, instance := range nonReadyEligibleInstances {
			op, err := containsOpWithInstanceTargetPrefix(instance, ops) // Error for this call is already checked above
			if err != nil {
				klog.Errorf("failed to check eligibility of instance %s", instance.Name)
				return nil, err
			}
			if op != nil {
				errorString = fmt.Sprintf("%s Instance %s busy with operation type %s\n", errorString, instance.Name, op.Type)
			} else {
				errorString = fmt.Sprintf("%s Instance %s is in state %s\n", errorString, instance.Name, instance.State)
			}
		}

		return nil, status.Error(codes.Aborted, errorString)

	}

	return readyEligibleInstances, nil
}

func (m *MultishareOpsManager) instanceNeedsExpand(ctx context.Context, share *file.Share, capacityNeeded int64) (bool, int64, error) {
	if share == nil {
		return false, 0, fmt.Errorf("empty share")
	}
	if share.Parent == nil {
		return false, 0, fmt.Errorf("parent missing from share %q", share.Name)
	}

	shares, err := m.cloud.File.ListShares(ctx, &file.ListFilter{Project: share.Parent.Project, Location: share.Parent.Location, InstanceName: share.Parent.Name})
	if err != nil {
		return false, 0, err
	}

	var sumShareBytes int64
	for _, s := range shares {
		sumShareBytes = sumShareBytes + s.CapacityBytes
	}

	remainingBytes := share.Parent.CapacityBytes - sumShareBytes
	if remainingBytes < capacityNeeded {
		alignBytes := util.AlignBytes(capacityNeeded+sumShareBytes, util.GbToBytes(share.Parent.CapacityStepSizeGb))
		targetBytes := util.Min(alignBytes, util.MaxMultishareInstanceSizeBytes)
		return true, targetBytes, nil
	}
	return false, 0, nil
}

func (m *MultishareOpsManager) checkAndStartInstanceOrShareExpandWorkflow(ctx context.Context, share *file.Share, reqBytes int64) (*Workflow, error) {
	m.Lock()
	defer m.Unlock()

	ops, err := m.listMultishareResourceRunningOps(ctx)
	if err != nil {
		return nil, err
	}

	expandShareOp, err := containsOpWithShareTarget(share, util.ShareUpdate, ops)
	if err != nil {
		return nil, err
	}
	if expandShareOp != nil {
		return &Workflow{share: share, opName: expandShareOp.Id, opType: expandShareOp.Type}, nil
	}

	// no existing share Expansion, proceed to instance check
	err = m.verifyNoRunningInstanceOrShareOpsForInstance(share.Parent, ops)
	if err != nil {
		klog.Infof("Instance %v has running share or instnace Op, aborting volume expansion.", share.Parent.Name)
		return nil, status.Error(codes.Aborted, err.Error())
	}

	instance, err := m.cloud.File.GetMultishareInstance(ctx, share.Parent)
	if err != nil {
		return nil, err
	}

	needExpand, targetBytes, err := m.instanceNeedsExpand(ctx, share, reqBytes-share.CapacityBytes)
	if err != nil {
		return nil, err
	}
	if needExpand {
		instance.CapacityBytes = targetBytes
		workflow, err := m.startInstanceWorkflow(ctx, &Workflow{instance: instance, opType: util.InstanceUpdate}, ops)
		return workflow, err
	}

	share.CapacityBytes = reqBytes
	return m.startShareWorkflow(ctx, &Workflow{share: share, opType: util.ShareUpdate}, ops)
}

func (m *MultishareOpsManager) startShareExpandWorkflowSafe(ctx context.Context, share *file.Share, reqBytes int64) (*Workflow, error) {
	m.Lock()
	defer m.Unlock()
	ops, err := m.listMultishareResourceRunningOps(ctx)
	if err != nil {
		return nil, err
	}

	share.CapacityBytes = reqBytes
	return m.startShareWorkflow(ctx, &Workflow{share: share, opType: util.ShareUpdate}, ops)
}

func (m *MultishareOpsManager) checkAndStartShareDeleteWorkflow(ctx context.Context, share *file.Share) (*Workflow, error) {
	m.Lock()
	defer m.Unlock()

	ops, err := m.listMultishareResourceRunningOps(ctx)
	if err != nil {
		return nil, err
	}

	// If we find a running delete share op, poll for that to complete.
	deleteShareOp, err := containsOpWithShareTarget(share, util.ShareDelete, ops)
	if err != nil {
		return nil, err
	}
	if deleteShareOp != nil {
		return &Workflow{share: share, opName: deleteShareOp.Id, opType: deleteShareOp.Type}, nil
	}

	return m.startShareWorkflow(ctx, &Workflow{share: share, opType: util.ShareDelete}, ops)
}

func (m *MultishareOpsManager) checkAndStartInstanceDeleteOrShrinkWorkflow(ctx context.Context, instance *file.MultishareInstance) (*Workflow, error) {
	m.Lock()
	defer m.Unlock()

	ops, err := m.listMultishareResourceRunningOps(ctx)
	if err != nil {
		return nil, err
	}

	err = m.verifyNoRunningInstanceOrShareOpsForInstance(instance, ops)
	if err != nil {
		return nil, err
	}

	// At this point no new share create or delete would be attempted since the driver has the lock.
	// 1. GET instance . if not found its a no-op return success.
	// 2. evaluate 0 shares.
	// 3. else evaluate instance size with share size sum.
	instance, err = m.cloud.File.GetMultishareInstance(ctx, instance)
	if err != nil {
		if file.IsNotFoundErr(err) {
			return nil, nil
		}
		return nil, err
	}

	shares, err := m.cloud.File.ListShares(ctx, &file.ListFilter{Project: instance.Project, Location: instance.Location, InstanceName: instance.Name})
	if err != nil {
		if file.IsNotFoundErr(err) {
			return nil, nil
		}
		return nil, err
	}

	// Check for delete
	if len(shares) == 0 {
		w, err := m.startInstanceWorkflow(ctx, &Workflow{instance: instance, opType: util.InstanceDelete}, ops)
		if err != nil {
			if file.IsNotFoundErr(err) {
				return nil, nil
			}
			return nil, err
		}
		return w, err
	}

	// check for shrink
	var totalShareCap int64
	for _, share := range shares {
		totalShareCap += share.CapacityBytes
	}
	if totalShareCap < instance.CapacityBytes && instance.CapacityBytes > util.MinMultishareInstanceSizeBytes {
		targetShrinkSizeBytes := util.AlignBytes(totalShareCap, util.GbToBytes(instance.CapacityStepSizeGb))
		targetShrinkSizeBytes = util.Max(targetShrinkSizeBytes, util.MinMultishareInstanceSizeBytes)
		if instance.CapacityBytes == targetShrinkSizeBytes {
			return nil, nil
		}

		instance.CapacityBytes = targetShrinkSizeBytes
		w, err := m.startInstanceWorkflow(ctx, &Workflow{instance: instance, opType: util.InstanceUpdate}, ops)
		if err != nil {
			if file.IsNotFoundErr(err) {
				return nil, nil
			}
			return nil, err
		}
		return w, err
	}

	return nil, nil
}

// listMultishareOps reports all running ops related to multishare instances and share resources. The op target is of the form "projects/<>/locations/<>/instances/<>" or "projects/<>/locations/<>/instances/<>/shares/<>"
func (m *MultishareOpsManager) listMultishareResourceRunningOps(ctx context.Context) ([]*OpInfo, error) {
	ops, err := m.cloud.File.ListOps(ctx, &file.ListFilter{Project: m.cloud.Project, Location: "-"})
	if err != nil {
		return nil, err
	}

	var finalops []*OpInfo
	for _, op := range ops {
		if op.Done {
			continue
		}

		if op.Metadata == nil {
			continue
		}

		var meta filev1beta1multishare.OperationMetadata
		if err := json.Unmarshal(op.Metadata, &meta); err != nil {
			klog.Errorf("Failed to parse metadata for op %s", op.Name)
			continue
		}

		if file.IsInstanceTarget(meta.Target) {
			finalops = append(finalops, &OpInfo{Id: op.Name, Target: meta.Target, Type: util.ConvertInstanceOpVerbToType(meta.Verb)})
		} else if file.IsShareTarget(meta.Target) {
			finalops = append(finalops, &OpInfo{Id: op.Name, Target: meta.Target, Type: util.ConvertShareOpVerbToType(meta.Verb)})
		}
		// TODO: Add other resource types if needed, when we support snapshot/backups.
	}
	return finalops, nil
}

// Whether there is any op with target that is the given share name
func containsOpWithShareName(shareName string, opType util.OperationType, ops []*OpInfo) *OpInfo {
	for _, op := range ops {
		// share names are expected to be unique in the cluster
		if op.Type == opType && strings.Contains(op.Target, shareName) {
			return op
		}
	}

	return nil
}

func containsOpWithShareTarget(share *file.Share, opType util.OperationType, ops []*OpInfo) (*OpInfo, error) {
	shareUri, err := file.GenerateShareURI(share)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to parse share handle, err: %v", err.Error())
	}

	for _, op := range ops {
		// share names are expected to be unique in the cluster
		if op.Type == opType && op.Target == shareUri {
			return op, nil
		}
	}

	return nil, nil
}

func containsOpWithInstanceTargetPrefix(instance *file.MultishareInstance, ops []*OpInfo) (*OpInfo, error) {
	instanceUri, err := file.GenerateMultishareInstanceURI(instance)
	if err != nil {
		return nil, err
	}
	for _, op := range ops {
		// For share targets (e.g projects/<>/locations/<>/instances/<>/shares/<>), explicity check with a "/", to avoid false positives of instances with same prefix name.
		if op.Target == instanceUri || strings.Contains(op.Target, instanceUri+"/") {
			return op, nil
		}
	}

	return nil, nil
}

// listMatchedInstances lists all instances under allowed regions in current project,
// but only matched instances will be returned.
func (m *MultishareOpsManager) listMatchedInstances(ctx context.Context, req *csi.CreateVolumeRequest, target *file.MultishareInstance, regions []string) ([]*file.MultishareInstance, error) {
	var instances []*file.MultishareInstance
	for _, region := range regions {
		regionalInstances, err := m.cloud.File.ListMultishareInstances(ctx, &file.ListFilter{Project: m.cloud.Project, Location: region})
		if err != nil {
			return nil, err
		}
		instances = append(instances, regionalInstances...)
	}

	var finalInstances []*file.MultishareInstance
	for _, i := range instances {
		matched, err := isMatchedInstance(i, target, req)
		if err != nil {
			return nil, err
		}
		klog.Infof("Found source instance %+v, comparing with target instance %+v and StorageClass parameters %v, matched = %t", *i, *target, req.GetParameters(), matched)
		if matched {
			finalInstances = append(finalInstances, i)
		}
	}
	return finalInstances, nil
}

// A source instance will be considered as "matched" with the target instance
// if and only if the following requirements were met:
//
//  1. Both source and target instance should have a label with key
//     "storage_gke_io_storage-class-id", and the value should be the same.
//
//  2. (Check if exists) The ip address of the target instance should be
//     within the ip range specified in "reserved-ipv4-cidr".
//
//  3. (Check if exists) The ip address of the target instance should be
//     within the ip range specified in "reserved-ip-range".
//
//  4. Both source and target instance should be in the same location.
//
//  5. Both source and target instance should be under the same tier.
//
//  6. Both source and target instance should be in the same VPC network.
//
//  7. Both source and target instance should have the same connect mode.
//
//  8. Both source and target instance should have the same KmsKeyName.
//
//  9. Both source and target instance should have a label with key
//     "gke_cluster_location", and the value should be the same.
//
//  10. Both source and target instance should have a label with key
//     "gke_cluster_name", and the value should be the same.
//
//  11. Both source and target instance should have the same FileSystem protocol.
func isMatchedInstance(source, target *file.MultishareInstance, req *csi.CreateVolumeRequest) (bool, error) {
	matchLabels := [3]string{util.ParamMultishareInstanceScLabelKey, TagKeyClusterLocation, TagKeyClusterName}
	for _, labelKey := range matchLabels {
		if _, ok := target.Labels[labelKey]; !ok {
			return false, fmt.Errorf("label %q missing in target instance %+v", labelKey, target)
		}
		if source.Labels[labelKey] != target.Labels[labelKey] {
			return false, nil
		}
	}
	params := req.GetParameters()
	if instanceCIDR, ok := params[ParamReservedIPV4CIDR]; ok {
		withinRange, err := IsIpWithinRange(source.Network.Ip, instanceCIDR)
		if err != nil {
			return false, err
		}
		if !withinRange {
			return false, nil
		}
	}

	if source.Protocol != target.Protocol {
		return false, nil
	}

	// Skip validation for parameter "reserved-ip-range" since it requires
	// extra compute api auth and not clear if it's required.
	if strings.EqualFold(source.Location, target.Location) &&
		strings.EqualFold(source.Tier, target.Tier) &&
		strings.EqualFold(source.Network.Name, target.Network.Name) &&
		strings.EqualFold(source.Network.ConnectMode, target.Network.ConnectMode) &&
		strings.EqualFold(source.KmsKeyName, target.KmsKeyName) {
		return true, nil
	}

	return false, nil
}
