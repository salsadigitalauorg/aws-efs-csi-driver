package driver

import (
	"context"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"

	"github.com/kubernetes-sigs/aws-efs-csi-driver/pkg/cloud"
)

type DirectoryProvisioner struct {
	mounter              Mounter
	cloud                cloud.Cloud
	osClient             OsClient
	deleteProvisionedDir bool
}

func (d DirectoryProvisioner) Provision(ctx context.Context, req *csi.CreateVolumeRequest, uid, gid int) (*csi.Volume, error) {
	var provisionedPath string

	var fileSystemId string
	volumeParams := req.GetParameters()
	if value, ok := volumeParams[FsId]; ok {
		if strings.TrimSpace(value) == "" {
			return nil, status.Errorf(codes.InvalidArgument, "Parameter %v cannot be empty", FsId)
		}
		fileSystemId = value
	} else {
		return nil, status.Errorf(codes.InvalidArgument, "Missing %v parameter", FsId)
	}
	klog.V(5).Infof("Provisioning directory on FileSystem %s...", fileSystemId)

	localCloud, roleArn, err := getCloud(d.cloud, req.GetSecrets())
	if err != nil {
		return nil, err
	}

	mountOptions, err := getMountOptions(ctx, localCloud, fileSystemId, roleArn)
	if err != nil {
		return nil, err
	}
	target := TempMountPathPrefix + "/" + uuid.New().String()
	if err := d.mounter.MakeDir(target); err != nil {
		return nil, status.Errorf(codes.Internal, "Could not create dir %q: %v", target, err)
	}
	if err := d.mounter.Mount(fileSystemId, target, "efs", mountOptions); err == nil {
		// Extract the basePath
		var basePath string
		if value, ok := volumeParams[BasePath]; ok {
			basePath = value
		}

		rootDirName := req.Name
		provisionedPath = basePath + "/" + rootDirName

		klog.V(5).Infof("Provisioning directory at path %s", provisionedPath)

		// Grab the required permissions
		perms := os.FileMode(0755)
		if value, ok := volumeParams[DirectoryPerms]; ok {
			parsedPerms, err := strconv.ParseUint(value, 8, 32)
			if err == nil {
				perms = os.FileMode(parsedPerms)
			}
		}

		klog.V(5).Infof("Provisioning directory with permissions %s", perms)

		provisionedDirectory := path.Join(target, provisionedPath)
		err := d.osClient.MkDirAllWithPerms(provisionedDirectory, perms, uid, gid)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "Could not provision directory: %v", err)
		}
	} else {
		return nil, status.Errorf(codes.Internal, "Could not mount %q at %q: %v", fileSystemId, target, err)
	}

	err = d.mounter.Unmount(target)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not unmount %q: %v", target, err)
	}
	err = d.osClient.RemoveAll(target)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not delete %q: %v", target, err)
	}

	return &csi.Volume{
		CapacityBytes: req.GetCapacityRange().GetRequiredBytes(),
		VolumeId:      fileSystemId + ":" + provisionedPath,
		VolumeContext: map[string]string{},
	}, nil
}

func (d DirectoryProvisioner) Delete(ctx context.Context, req *csi.DeleteVolumeRequest) (e error) {
	if !d.deleteProvisionedDir {
		return nil
	}
	fileSystemId, subpath, _, _ := parseVolumeId(req.GetVolumeId())

	localCloud, roleArn, err := getCloud(d.cloud, req.GetSecrets())
	if err != nil {
		return err
	}

	mountOptions, err := getMountOptions(ctx, localCloud, fileSystemId, roleArn)
	if err != nil {
		return err
	}

	target := TempMountPathPrefix + "/" + uuid.New().String()
	if err := d.mounter.MakeDir(target); err != nil {
		return status.Errorf(codes.Internal, "Could not create dir %q: %v", target, err)
	}

	defer func() {
		if err := d.mounter.Unmount(target); err != nil {
			e = status.Errorf(codes.Internal, "Could not unmount %q: %v", target, err)
		}
	}()

	defer func() {
		if err := d.osClient.RemoveAll(target); err != nil {
			e = status.Errorf(codes.Internal, "Could not delete %q: %v", target, err)
		}
	}()

	if err := d.mounter.Mount(fileSystemId, target, "efs", mountOptions); err != nil {
		d.osClient.Remove(target)
		return status.Errorf(codes.Internal, "Could not mount %q at %q: %v", fileSystemId, target, err)
	}
	if err := d.osClient.RemoveAll(target + subpath); err != nil {
		return status.Errorf(codes.Internal, "Could not delete directory %q: %v", subpath, err)
	}

	return nil
}
