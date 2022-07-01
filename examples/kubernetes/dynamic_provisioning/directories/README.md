## Dynamic Provisioning
This example shows how to create a dynamically provisioned volume created through a directory on the file system and a Persistent Volume Claim (PVC) and consume it from a pod.

**Note**: this example requires Kubernetes v1.17+ and driver version >= 1.2.0.

### Edit [StorageClass](specstorageclass.yaml)

```
kind: StorageClass
apiVersion: storage.k8s.io/v1
metadata:
  name: efs-sc
provisioner: efs.csi.aws.com
mountOptions:
  - tls
parameters:
  provisioningMode: efs-dir
  fileSystemId: fs-92107410
  directoryPerms: "700"
  gidRangeStart: "1000"
  gidRangeEnd: "2000"
  basePath: "/dynamic_provisioning"
```
* `provisioningMode` - The type of volume to be provisioned by efs.
* `fileSystemId` - The file system under which the directory is to be created.
* `directoryPerms` - Directory Permissions of the root directory created by Access Point.
* `gidRangeStart` (Optional) - Starting range of Posix Group ID to be applied onto the root directory of the access point. Default value is 50000. 
* `gidRangeEnd` (Optional) - Ending range of Posix Group ID. Default value is 7000000.
* `basePath` (Optional) - Path on the file system under which directory is created. If path is not provided, access points root directory are created under the root of the file system.

### Deploy the Example
Create storage class, persistent volume claim (PVC) and the pod which consumes PV:
```sh
>> kubectl apply -f examples/kubernetes/dynamic_provisioning/directories/specs/storageclass.yaml
>> kubectl apply -f examples/kubernetes/dynamic_provisioning/directories/specs/pod.yaml
```

### Check EFS filesystem is used
After the objects are created, verify that pod is running:

```sh
>> kubectl get pods
```

Also you can verify that data is written onto EFS filesystem:

```sh
>> kubectl exec -ti efs-app -- tail -f /data/out
```
