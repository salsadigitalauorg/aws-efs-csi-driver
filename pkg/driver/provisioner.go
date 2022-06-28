package driver

import (
	"context"
	"os"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"

	"github.com/kubernetes-sigs/aws-efs-csi-driver/pkg/cloud"
)

type Provisioner interface {
	Provision(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.Volume, error)
	Delete(ctx context.Context, req *csi.DeleteVolumeRequest) error
}

type AccessPointProvisioner struct {
	tags                     map[string]string
	cloud                    cloud.Cloud
	gidAllocator             *GidAllocator
	deleteAccessPointRootDir bool
	mounter                  Mounter
}

func getProvisioners(tags map[string]string, cloud cloud.Cloud, gidAllocator *GidAllocator, deleteAccessPointRootDir bool, mounter Mounter) map[string]Provisioner {
	return map[string]Provisioner{
		AccessPointMode: AccessPointProvisioner{
			tags:                     tags,
			cloud:                    cloud,
			gidAllocator:             gidAllocator,
			deleteAccessPointRootDir: deleteAccessPointRootDir,
			mounter:                  mounter,
		},
	}
}

func (a AccessPointProvisioner) Provision(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.Volume, error) {
	volumeParams := req.GetParameters()
	volName := req.GetName()
	if volName == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume name not provided")
	}

	// Volume size is required to match PV to PVC by k8s.
	// Volume size is not consumed by EFS for any purposes.
	volSize := req.GetCapacityRange().GetRequiredBytes()

	var (
		azName   string
		basePath string
		err      error
		gid      int
		gidMin   int
		gidMax   int
		roleArn  string
		uid      int
	)

	// Create tags
	tags := map[string]string{
		DefaultTagKey: DefaultTagValue,
	}

	// Append input tags to default tag
	if len(a.tags) != 0 {
		for k, v := range a.tags {
			tags[k] = v
		}
	}

	accessPointsOptions := &cloud.AccessPointOptions{
		CapacityGiB: volSize,
		Tags:        tags,
	}

	if value, ok := volumeParams[FsId]; ok {
		if strings.TrimSpace(value) == "" {
			return nil, status.Errorf(codes.InvalidArgument, "Parameter %v cannot be empty", FsId)
		}
		accessPointsOptions.FileSystemId = value
	} else {
		return nil, status.Errorf(codes.InvalidArgument, "Missing %v parameter", FsId)
	}

	uid = -1
	if value, ok := volumeParams[Uid]; ok {
		uid, err = strconv.Atoi(value)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "Failed to parse invalid %v: %v", Uid, err)
		}
		if uid < 0 {
			return nil, status.Errorf(codes.InvalidArgument, "%v must be greater or equal than 0", Uid)
		}
	}

	gid = -1
	if value, ok := volumeParams[Gid]; ok {
		gid, err = strconv.Atoi(value)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "Failed to parse invalid %v: %v", Gid, err)
		}
		if uid < 0 {
			return nil, status.Errorf(codes.InvalidArgument, "%v must be greater or equal than 0", Gid)
		}
	}

	if value, ok := volumeParams[GidMin]; ok {
		gidMin, err = strconv.Atoi(value)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "Failed to parse invalid %v: %v", GidMin, err)
		}
		if gidMin <= 0 {
			return nil, status.Errorf(codes.InvalidArgument, "%v must be greater than 0", GidMin)
		}
	}

	if value, ok := volumeParams[GidMax]; ok {
		// Ensure GID min is provided with GID max
		if gidMin == 0 {
			return nil, status.Errorf(codes.InvalidArgument, "Missing %v parameter", GidMin)
		}
		gidMax, err = strconv.Atoi(value)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "Failed to parse invalid %v: %v", GidMax, err)
		}
		if gidMax <= gidMin {
			return nil, status.Errorf(codes.InvalidArgument, "%v must be greater than %v", GidMax, GidMin)
		}
	} else {
		// Ensure GID max is provided with GID min
		if gidMin != 0 {
			return nil, status.Errorf(codes.InvalidArgument, "Missing %v parameter", GidMax)
		}
	}

	// Assign default GID ranges if not provided
	if gidMin == 0 && gidMax == 0 {
		gidMin = DefaultGidMin
		gidMax = DefaultGidMax
	}

	if value, ok := volumeParams[DirectoryPerms]; ok {
		accessPointsOptions.DirectoryPerms = value
	}

	if value, ok := volumeParams[BasePath]; ok {
		basePath = value
	}

	// Storage class parameter `az` will be used to fetch preferred mount target for cross account mount.
	// If the `az` storage class parameter is not provided, a random mount target will be picked for mounting.
	// This storage class parameter different from `az` mount option provided by efs-utils https://github.com/aws/efs-utils/blob/v1.31.1/src/mount_efs/__init__.py#L195
	// The `az` mount option provided by efs-utils is used for cross az mount or to provide az of efs one zone file system mount within the same aws-account.
	// To make use of the `az` mount option, add it under storage class's `mountOptions` section. https://kubernetes.io/docs/concepts/storage/storage-classes/#mount-options
	if value, ok := volumeParams[AzName]; ok {
		azName = value
	}

	localCloud, roleArn, err := a.getCloud(req.GetSecrets())
	if err != nil {
		return nil, err
	}

	// Check if file system exists. Describe FS handles appropriate error codes
	if _, err = localCloud.DescribeFileSystem(ctx, accessPointsOptions.FileSystemId); err != nil {
		if err == cloud.ErrAccessDenied {
			return nil, status.Errorf(codes.Unauthenticated, "Access Denied. Please ensure you have the right AWS permissions: %v", err)
		}
		if err == cloud.ErrNotFound {
			return nil, status.Errorf(codes.InvalidArgument, "File System does not exist: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "Failed to fetch File System info: %v", err)
	}

	var allocatedGid int
	if uid == -1 || gid == -1 {
		allocatedGid, err = a.gidAllocator.getNextGid(accessPointsOptions.FileSystemId, gidMin, gidMax)
		if err != nil {
			return nil, err
		}
	}
	if uid == -1 {
		uid = allocatedGid
	}
	if gid == -1 {
		gid = allocatedGid
	}

	rootDirName := volName
	rootDir := basePath + "/" + rootDirName

	accessPointsOptions.Uid = int64(uid)
	accessPointsOptions.Gid = int64(gid)
	accessPointsOptions.DirectoryPath = rootDir

	accessPointId, err := localCloud.CreateAccessPoint(ctx, volName, accessPointsOptions)
	if err != nil {
		a.gidAllocator.releaseGid(accessPointsOptions.FileSystemId, gid)
		if err == cloud.ErrAccessDenied {
			return nil, status.Errorf(codes.Unauthenticated, "Access Denied. Please ensure you have the right AWS permissions: %v", err)
		}
		if err == cloud.ErrAlreadyExists {
			return nil, status.Errorf(codes.AlreadyExists, "Access Point already exists")
		}
		return nil, status.Errorf(codes.Internal, "Failed to create Access point in File System %v : %v", accessPointsOptions.FileSystemId, err)
	}

	volContext := map[string]string{}

	// Fetch mount target Ip for cross-account mount
	if roleArn != "" {
		mountTarget, err := localCloud.DescribeMountTargets(ctx, accessPointsOptions.FileSystemId, azName)
		if err != nil {
			klog.Warningf("Failed to describe mount targets for file system %v. Skip using `mounttargetip` mount option: %v", accessPointsOptions.FileSystemId, err)
		} else {
			volContext[MountTargetIp] = mountTarget.IPAddress
		}
	}

	return &csi.Volume{
		CapacityBytes: volSize,
		VolumeId:      accessPointsOptions.FileSystemId + "::" + accessPointId.AccessPointId,
		VolumeContext: volContext,
	}, nil
}

func (a AccessPointProvisioner) Delete(ctx context.Context, req *csi.DeleteVolumeRequest) error {
	localCloud, roleArn, err := a.getCloud(req.GetSecrets())
	if err != nil {
		return err
	}

	fileSystemId, _, accessPointId, _ := parseVolumeId(req.GetVolumeId())
	// Delete access point root directory if delete-access-point-root-dir is set.
	if a.deleteAccessPointRootDir {
		// Check if Access point exists.
		// If access point exists, retrieve its root directory and delete it/
		accessPoint, err := localCloud.DescribeAccessPoint(ctx, accessPointId)
		if err != nil {
			if err == cloud.ErrAccessDenied {
				return status.Errorf(codes.Unauthenticated, "Access Denied. Please ensure you have the right AWS permissions: %v", err)
			}
			if err == cloud.ErrNotFound {
				klog.V(5).Infof("DeleteVolume: Access Point %v not found, returning success", accessPointId)
				return nil
			}
			return status.Errorf(codes.Internal, "Could not get describe Access Point: %v , error: %v", accessPointId, err)
		}

		//Mount File System at it root and delete access point root directory
		mountOptions := []string{"tls", "iam"}
		if roleArn != "" {
			mountTarget, err := localCloud.DescribeMountTargets(ctx, fileSystemId, "")

			if err == nil {
				mountOptions = append(mountOptions, MountTargetIp+"="+mountTarget.IPAddress)
			} else {
				klog.Warningf("Failed to describe mount targets for file system %v. Skip using `mounttargetip` mount option: %v", fileSystemId, err)
			}
		}

		target := TempMountPathPrefix + "/" + accessPointId
		if err := a.mounter.MakeDir(target); err != nil {
			return status.Errorf(codes.Internal, "Could not create dir %q: %v", target, err)
		}
		if err := a.mounter.Mount(fileSystemId, target, "efs", mountOptions); err != nil {
			os.Remove(target)
			return status.Errorf(codes.Internal, "Could not mount %q at %q: %v", fileSystemId, target, err)
		}
		err = os.RemoveAll(target + accessPoint.AccessPointRootDir)
		if err != nil {
			return status.Errorf(codes.Internal, "Could not delete access point root directory %q: %v", accessPoint.AccessPointRootDir, err)
		}
		err = a.mounter.Unmount(target)
		if err != nil {
			return status.Errorf(codes.Internal, "Could not unmount %q: %v", target, err)
		}
		err = os.RemoveAll(target)
		if err != nil {
			return status.Errorf(codes.Internal, "Could not delete %q: %v", target, err)
		}
	}

	// Delete access point
	if err = localCloud.DeleteAccessPoint(ctx, accessPointId); err != nil {
		if err == cloud.ErrAccessDenied {
			return status.Errorf(codes.Unauthenticated, "Access Denied. Please ensure you have the right AWS permissions: %v", err)
		}
		if err == cloud.ErrNotFound {
			klog.V(5).Infof("DeleteVolume: Access Point not found, returning success")
			return nil
		}
		return status.Errorf(codes.Internal, "Failed to Delete volume %v: %v", req.GetVolumeId(), err)
	}

	return nil
}

func (a AccessPointProvisioner) getCloud(secrets map[string]string) (cloud.Cloud, string, error) {

	var localCloud cloud.Cloud
	var roleArn string
	var err error

	// Fetch aws role ARN for cross account mount from CSI secrets. Link to CSI secrets below
	// https://kubernetes-csi.github.io/docs/secrets-and-credentials.html#csi-operation-secrets
	if value, ok := secrets[RoleArn]; ok {
		roleArn = value
	}

	if roleArn != "" {
		localCloud, err = cloud.NewCloudWithRole(roleArn)
		if err != nil {
			return nil, "", status.Errorf(codes.Unauthenticated, "Unable to initialize aws cloud: %v. Please verify role has the correct AWS permissions for cross account mount", err)
		}
	} else {
		localCloud = a.cloud
	}

	return localCloud, roleArn, nil
}
