/*
Copyright 2016 The Rook Authors. All rights reserved.

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

// Package osd for the Ceph OSDs.
package osd

import (
	"fmt"
	"path"
	"strconv"

	"github.com/pkg/errors"
	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	opmon "github.com/rook/rook/pkg/operator/ceph/cluster/mon"
	"github.com/rook/rook/pkg/operator/ceph/cluster/osd/config"
	opconfig "github.com/rook/rook/pkg/operator/ceph/config"
	"github.com/rook/rook/pkg/operator/ceph/controller"
	"github.com/rook/rook/pkg/operator/k8sutil"
	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	rookBinariesMountPath                         = "/rook"
	rookBinariesVolumeName                        = "rook-binaries"
	activateOSDVolumeName                         = "activate-osd"
	activateOSDMountPath                          = "/var/lib/ceph/osd/ceph-"
	blockPVCMapperInitContainer                   = "blkdevmapper"
	blockEncryptionOpenInitContainer              = "encryption-open"
	blockEncryptionOpenMetadataInitContainer      = "encryption-open-metadata"
	blockEncryptionOpenWalInitContainer           = "encryption-open-wal"
	blockPVCMapperEncryptionInitContainer         = "blkdevmapper-encryption"
	blockPVCMapperEncryptionMetadataInitContainer = "blkdevmapper-metadata-encryption"
	blockPVCMapperEncryptionWalInitContainer      = "blkdevmapper-wal-encryption"
	blockPVCMetadataMapperInitContainer           = "blkdevmapper-metadata"
	blockPVCWalMapperInitContainer                = "blkdevmapper-wal"
	activatePVCOSDInitContainer                   = "activate"
	expandPVCOSDInitContainer                     = "expand-bluefs"
	expandEncryptedPVCOSDInitContainer            = "expand-encrypted-bluefs"
	encryptedPVCStatusOSDInitContainer            = "encrypted-block-status"
	encryptionKeyFileName                         = "luks_key"
	// DmcryptBlockType is a portion of the device mapper name for the encrypted OSD on PVC block.db (rocksdb db)
	DmcryptBlockType = "block-dmcrypt"
	// DmcryptMetadataType is a portion of the device mapper name for the encrypted OSD on PVC block
	DmcryptMetadataType = "db-dmcrypt"
	// DmcryptWalType is a portion of the device mapper name for the encrypted OSD on PVC wal
	DmcryptWalType      = "wal-dmcrypt"
	dmcryptBlockName    = "block"
	dmcryptMetadataName = "block.db"
	dmcryptWalName      = "block.wal"
)

const (
	activateOSDCode = `
set -ex

OSD_ID=%s
OSD_UUID=%s
OSD_STORE_FLAG="%s"
OSD_DATA_DIR=/var/lib/ceph/osd/ceph-"$OSD_ID"
CV_MODE=%s
DEVICE=%s
METADATA_DEVICE="$%s"
WAL_DEVICE="$%s"

# active the osd with ceph-volume
if [[ "$CV_MODE" == "lvm" ]]; then
	TMP_DIR=$(mktemp -d)

	# activate osd
	ceph-volume "$CV_MODE" activate --no-systemd "$OSD_STORE_FLAG" "$OSD_ID" "$OSD_UUID"

	# copy the tmpfs directory to a temporary directory
	# this is needed because when the init container exits, the tmpfs goes away and its content with it
	# this will result in the emptydir to be empty when accessed by the main osd container
	cp --verbose --no-dereference "$OSD_DATA_DIR"/* "$TMP_DIR"/

	# unmount the tmpfs since we don't need it anymore
	umount "$OSD_DATA_DIR"

	# copy back the content of the tmpfs into the original osd directory
	cp --verbose --no-dereference "$TMP_DIR"/* "$OSD_DATA_DIR"

	# retain ownership of files to the ceph user/group
	chown --verbose --recursive ceph:ceph "$OSD_DATA_DIR"

	# remove the temporary directory
	rm --recursive --force "$TMP_DIR"
else
	ARGS=(--device ${DEVICE} --no-systemd --no-tmpfs)
	if [ -n "$METADATA_DEVICE" ]; then
		ARGS+=(--block.db ${METADATA_DEVICE})
	fi
	if [ -n "$WAL_DEVICE" ]; then
		ARGS+=(--block.wal ${WAL_DEVICE})
	fi
	# ceph-volume raw mode only supports bluestore so we don't need to pass a store flag
	ceph-volume "$CV_MODE" activate "${ARGS[@]}"
fi

`

	openEncryptedBlock = `
set -xe

KEY_FILE_PATH=%s
BLOCK_PATH=%s
DM_NAME=%s
DM_PATH=%s

function open_encrypted_block {
	echo "Opening encrypted device $BLOCK_PATH at $DM_PATH"
	cryptsetup luksOpen --verbose --disable-keyring --allow-discards --key-file "$KEY_FILE_PATH" "$BLOCK_PATH" "$DM_NAME"
}

if [ -b "$DM_PATH" ]; then
	echo "Encrypted device $BLOCK_PATH already opened at $DM_PATH"
	for field in $(dmsetup table "$DM_NAME"); do
		if [[ "$field" =~ ^[0-9]+\:[0-9]+ ]]; then
			underlaying_block="/sys/dev/block/$field"
			if [ ! -d "$underlaying_block" ]; then
				echo "Underlaying block device $underlaying_block of crypt $DM_NAME disappeared!"
				echo "Removing stale dm device $DM_NAME"
				dmsetup remove --force "$DM_NAME"
				open_encrypted_block
			fi
		fi
	done
else
	open_encrypted_block
fi
`
)

// OSDs on PVC using a certain fast storage class need to do some tuning
var defaultTuneFastSettings = []string{
	"--osd-op-num-threads-per-shard=2",            // Default value of osd_op_num_threads_per_shard for SSDs
	"--osd-op-num-shards=8",                       // Default value of osd_op_num_shards for SSDs
	"--osd-recovery-sleep=0",                      // Time in seconds to sleep before next recovery or backfill op for SSDs
	"--osd-snap-trim-sleep=0",                     // Time in seconds to sleep before next snap trim for SSDs
	"--osd-delete-sleep=0",                        // Time in seconds to sleep before next removal transaction for SSDs
	"--bluestore-min-alloc-size=4096",             // Default min_alloc_size value for SSDs
	"--bluestore-prefer-deferred-size=0",          // Default value of bluestore_prefer_deferred_size for SSDs
	"--bluestore-compression-min-blob-size=8192",  // Default value of bluestore_compression_min_blob_size for SSDs
	"--bluestore-compression-max-blob-size=65536", // Default value of bluestore_compression_max_blob_size for SSDs
	"--bluestore-max-blob-size=65536",             // Default value of bluestore_max_blob_size for SSDs
	"--bluestore-cache-size=3221225472",           // Default value of bluestore_cache_size for SSDs
	"--bluestore-throttle-cost-per-io=4000",       // Default value of bluestore_throttle_cost_per_io for SSDs
	"--bluestore-deferred-batch-ops=16",           // Default value of bluestore_deferred_batch_ops for SSDs
}

// OSDs on PVC using a certain slow storage class need to do some tuning
var defaultTuneSlowSettings = []string{
	"--osd-recovery-sleep=0.1", // Time in seconds to sleep before next recovery or backfill op
	"--osd-snap-trim-sleep=2",  // Time in seconds to sleep before next snap trim
	"--osd-delete-sleep=2",     // Time in seconds to sleep before next removal transaction
}

func (c *Cluster) makeDeployment(osdProps osdProperties, osd OSDInfo, provisionConfig *provisionConfig) (*apps.Deployment, error) {
	// If running on Octopus, we don't need to use the host PID namespace
	var hostPID = !c.clusterInfo.CephVersion.IsAtLeastOctopus()
	deploymentName := fmt.Sprintf(osdAppNameFmt, osd.ID)
	replicaCount := int32(1)
	volumeMounts := controller.CephVolumeMounts(provisionConfig.DataPathMap, false)
	configVolumeMounts := controller.RookVolumeMounts(provisionConfig.DataPathMap, false)
	// When running on PVC, the OSDs don't need a bindmount on dataDirHostPath, only the monitors do
	if osdProps.onPVC() {
		c.spec.DataDirHostPath = ""
	}
	volumes := controller.PodVolumes(provisionConfig.DataPathMap, c.spec.DataDirHostPath, false)
	failureDomainValue := osdProps.crushHostname
	doConfigInit := true       // initialize ceph.conf in init container?
	doBinaryCopyInit := true   // copy tini and rook binaries in an init container?
	doActivateOSDInit := false // run an init container to activate the osd?

	// If CVMode is empty, this likely means we upgraded Rook
	// This property did not exist before so we need to initialize it
	// This property is used for both PVC and non-PVC use case
	if osd.CVMode == "" {
		osd.CVMode = "lvm"
	}

	dataDir := k8sutil.DataDir
	// Create volume config for /dev so the pod can access devices on the host
	// Only valid when running OSD with LVM mode
	if osd.CVMode == "lvm" {
		devVolume := v1.Volume{Name: "devices", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/dev"}}}
		volumes = append(volumes, devVolume)
		devMount := v1.VolumeMount{Name: "devices", MountPath: "/dev"}
		volumeMounts = append(volumeMounts, devMount)
	}

	// If the OSD runs on PVC
	if osdProps.onPVC() {
		// Create volume config for PVCs
		volumes = append(volumes, getPVCOSDVolumes(&osdProps)...)
		// If encrypted let's add the secret key mount path
		if osdProps.encrypted && osd.CVMode == "raw" {
			encryptedVol, _ := getEncryptionVolume(osdProps.pvc.ClaimName)
			volumes = append(volumes, encryptedVol)
		}
	}

	if len(volumes) == 0 {
		return nil, errors.New("empty volumes")
	}

	storeType := config.Bluestore
	osdID := strconv.Itoa(osd.ID)
	tiniEnvVar := v1.EnvVar{Name: "TINI_SUBREAPER", Value: ""}
	envVars := append(c.getConfigEnvVars(osdProps, dataDir), []v1.EnvVar{
		tiniEnvVar,
	}...)
	envVars = append(envVars, k8sutil.ClusterDaemonEnvVars(c.spec.CephVersion.Image)...)
	envVars = append(envVars, []v1.EnvVar{
		{Name: "ROOK_OSD_UUID", Value: osd.UUID},
		{Name: "ROOK_OSD_ID", Value: osdID},
		{Name: "ROOK_OSD_STORE_TYPE", Value: storeType},
		{Name: "ROOK_CEPH_MON_HOST",
			ValueFrom: &v1.EnvVarSource{
				SecretKeyRef: &v1.SecretKeySelector{LocalObjectReference: v1.LocalObjectReference{
					Name: "rook-ceph-config"},
					Key: "mon_host"}}},
		{Name: "CEPH_ARGS", Value: "-m $(ROOK_CEPH_MON_HOST)"},
	}...)
	configEnvVars := append(c.getConfigEnvVars(osdProps, dataDir), []v1.EnvVar{
		tiniEnvVar,
		{Name: "ROOK_OSD_ID", Value: osdID},
		{Name: "ROOK_CEPH_VERSION", Value: c.clusterInfo.CephVersion.CephVersionFormatted()},
		{Name: "ROOK_IS_DEVICE", Value: "true"},
	}...)

	var command []string
	var args []string
	// If the OSD was prepared with ceph-volume and running on PVC and using the LVM mode
	if osdProps.onPVC() && osd.CVMode == "lvm" {
		// if the osd was provisioned by ceph-volume, we need to launch it with rook as the parent process
		command = []string{path.Join(rookBinariesMountPath, "tini")}
		args = []string{
			"--", path.Join(rookBinariesMountPath, "rook"),
			"ceph", "osd", "start",
			"--",
			"--foreground",
			"--id", osdID,
			"--fsid", c.clusterInfo.FSID,
			"--cluster", "ceph",
			"--setuser", "ceph",
			"--setgroup", "ceph",
			fmt.Sprintf("--crush-location=%s", osd.Location),
		}
	} else if osdProps.onPVC() && osd.CVMode == "raw" {
		doBinaryCopyInit = false
		doConfigInit = false
		command = []string{"ceph-osd"}
		args = []string{
			"--foreground",
			"--id", osdID,
			"--fsid", c.clusterInfo.FSID,
			"--setuser", "ceph",
			"--setgroup", "ceph",
			fmt.Sprintf("--crush-location=%s", osd.Location),
		}
	} else {
		doBinaryCopyInit = false
		doConfigInit = false
		doActivateOSDInit = true
		command = []string{"ceph-osd"}
		args = []string{
			"--foreground",
			"--id", osdID,
			"--fsid", c.clusterInfo.FSID,
			"--setuser", "ceph",
			"--setgroup", "ceph",
			fmt.Sprintf("--crush-location=%s", osd.Location),
		}
	}

	// If the OSD runs on PVC
	if osdProps.onPVC() {
		// add the PVC size to the pod spec so that if the size changes the OSD will be restarted and pick up the change
		envVars = append(envVars, v1.EnvVar{Name: "ROOK_OSD_PVC_SIZE", Value: osdProps.pvcSize})

		// Append slow tuning flag if necessary
		if osdProps.tuneSlowDeviceClass {
			args = append(args, defaultTuneSlowSettings...)
		} else if osdProps.tuneFastDeviceClass { // Append fast tuning flag if necessary
			args = append(args, defaultTuneFastSettings...)
		}
	}

	// The osd itself needs to talk to udev to report information about the device (vendor/serial etc)
	udevVolume, udevVolumeMount := getUdevVolume()
	volumes = append(volumes, udevVolume)
	volumeMounts = append(volumeMounts, udevVolumeMount)

	// If the PV is encrypted let's mount the device mapper path
	if osdProps.encrypted {
		dmVol, dmVolMount := getDeviceMapperVolume()
		volumes = append(volumes, dmVol)
		volumeMounts = append(volumeMounts, dmVolMount)
	}

	// Add the volume to the spec and the mount to the daemon container
	copyBinariesVolume, copyBinariesContainer := c.getCopyBinariesContainer()
	if doBinaryCopyInit {
		volumes = append(volumes, copyBinariesVolume)
		volumeMounts = append(volumeMounts, copyBinariesContainer.VolumeMounts[0])
	}

	// Add the volume to the spec and the mount to the daemon container
	// so that it can pick the already mounted/activated osd metadata path
	// This container will activate the OSD and place the activated filesinto an empty dir
	// The empty dir will be shared by the "activate-osd" pod and the "osd" main pod
	activateOSDVolume, activateOSDContainer := c.getActivateOSDInitContainer(osdID, osd, osdProps)
	if doActivateOSDInit {
		volumes = append(volumes, activateOSDVolume)
		volumeMounts = append(volumeMounts, activateOSDContainer.VolumeMounts[0])
	}

	args = append(args, opconfig.LoggingFlags()...)
	args = append(args, osdOnSDNFlag(c.spec.Network)...)

	osdDataDirPath := activateOSDMountPath + osdID
	if osdProps.onPVC() && osd.CVMode == "lvm" {
		// Let's use the old bridge for these lvm based pvc osds
		volumeMounts = append(volumeMounts, getPvcOSDBridgeMount(osdProps.pvc.ClaimName))
		envVars = append(envVars, pvcBackedOSDEnvVar("true"))
		envVars = append(envVars, blockPathEnvVariable(osd.BlockPath))
		envVars = append(envVars, cvModeEnvVariable(osd.CVMode))
		envVars = append(envVars, lvBackedPVEnvVar(strconv.FormatBool(osd.LVBackedPV)))
	}

	if osdProps.onPVC() && osd.CVMode == "raw" {
		volumeMounts = append(volumeMounts, getPvcOSDBridgeMountActivate(osdDataDirPath, osdProps.pvc.ClaimName))
		envVars = append(envVars, pvcBackedOSDEnvVar("true"))
		envVars = append(envVars, blockPathEnvVariable(osd.BlockPath))
		envVars = append(envVars, cvModeEnvVariable(osd.CVMode))
	}

	// We cannot go un-privileged until we have a bindmount for logs and crash
	// OpenShift requires privileged containers for that
	// If we remove those OSD on PVC with raw mode won't need to be privileged
	// We could try to run as ceph too, more investigations needed
	privileged := true
	runAsUser := int64(0)
	readOnlyRootFilesystem := false
	securityContext := &v1.SecurityContext{
		Privileged:             &privileged,
		RunAsUser:              &runAsUser,
		ReadOnlyRootFilesystem: &readOnlyRootFilesystem,
	}

	// needed for luksOpen synchronization when devices are encrypted and the osd is prepared with LVM
	hostIPC := osdProps.storeConfig.EncryptedDevice

	initContainers := make([]v1.Container, 0, 4)
	if doConfigInit {
		initContainers = append(initContainers,
			v1.Container{
				Args:            []string{"ceph", "osd", "init"},
				Name:            controller.ConfigInitContainerName,
				Image:           k8sutil.MakeRookImage(c.rookVersion),
				VolumeMounts:    configVolumeMounts,
				Env:             configEnvVars,
				SecurityContext: securityContext,
			})
	}
	if doBinaryCopyInit {
		initContainers = append(initContainers, *copyBinariesContainer)
	}
	if osdProps.onPVC() && osd.CVMode == "lvm" {
		initContainers = append(initContainers, c.getPVCInitContainer(osdProps))
	}

	if osdProps.onPVC() && osd.CVMode == "raw" {
		if osdProps.encrypted {
			// Open the encrypted disk
			initContainers = append(initContainers, c.getPVCEncryptionOpenInitContainerActivate(osdProps)...)
			// Copy the encrypted block to the osd data location, e,g: /var/lib/ceph/osd/ceph-0/block
			initContainers = append(initContainers, c.getPVCEncryptionInitContainerActivate(osdDataDirPath, osdProps)...)
			// Print the encrypted block status
			initContainers = append(initContainers, c.getEncryptedStatusPVCInitContainer(osdDataDirPath, osdProps))
			// Resize the encrypted device if necessary, this must be done after the encrypted block is opened
			initContainers = append(initContainers, c.getExpandEncryptedPVCInitContainer(osdDataDirPath, osdProps))
		} else {
			initContainers = append(initContainers, c.getPVCInitContainerActivate(osdDataDirPath, osdProps))
			if osdProps.onPVCWithMetadata() {
				initContainers = append(initContainers, c.getPVCMetadataInitContainerActivate(osdDataDirPath, osdProps))
			}
			if osdProps.onPVCWithWal() {
				initContainers = append(initContainers, c.getPVCWalInitContainerActivate(osdDataDirPath, osdProps))
			}
		}
		initContainers = append(initContainers, c.getActivatePVCInitContainer(osdProps, osdID))
		initContainers = append(initContainers, c.getExpandPVCInitContainer(osdProps, osdID))

	}
	if doActivateOSDInit {
		initContainers = append(initContainers, *activateOSDContainer)
	}

	// For OSD on PVC with LVM the directory does not exist yet
	// It gets created by the 'ceph-volume lvm activate' command
	//
	// 	So OSD non-PVC the directory has been created by the 'activate' container already and has chown it
	// So we don't need to chown it again
	dataPath := ""

	// Raw mode on PVC needs this path so that OSD's metadata files can be chown after 'ceph-bluestore-tool' ran
	if osd.CVMode == "raw" && osdProps.onPVC() {
		dataPath = activateOSDMountPath + osdID
	}

	// Doing a chown in a post start lifecycle hook does not reliably complete before the OSD
	// process starts, which can cause the pod to fail without the lifecycle hook's chown command
	// completing. It can take an arbitrarily long time for a pod restart to successfully chown the
	// directory. This is a race condition for all OSDs; therefore, do this in an init container.
	// See more discussion here: https://github.com/rook/rook/pull/3594#discussion_r312279176
	initContainers = append(initContainers,
		controller.ChownCephDataDirsInitContainer(
			opconfig.DataPathMap{ContainerDataDir: dataPath},
			c.spec.CephVersion.Image,
			volumeMounts,
			osdProps.resources,
			securityContext,
		))

	podTemplateSpec := v1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Name:   AppName,
			Labels: c.getOSDLabels(osd, failureDomainValue, osdProps.portable),
		},
		Spec: v1.PodSpec{
			RestartPolicy:      v1.RestartPolicyAlways,
			ServiceAccountName: serviceAccountName,
			HostNetwork:        c.spec.Network.IsHost(),
			HostPID:            hostPID,
			HostIPC:            hostIPC,
			PriorityClassName:  cephv1.GetOSDPriorityClassName(c.spec.PriorityClassNames),
			InitContainers:     initContainers,
			Containers: []v1.Container{
				{
					Command:         command,
					Args:            args,
					Name:            "osd",
					Image:           c.spec.CephVersion.Image,
					VolumeMounts:    volumeMounts,
					Env:             envVars,
					Resources:       osdProps.resources,
					SecurityContext: securityContext,
					LivenessProbe:   controller.GenerateLivenessProbeExecDaemon(opconfig.OsdType, osdID),
				},
			},
			Volumes:       volumes,
			SchedulerName: osdProps.schedulerName,
		},
	}

	// If the liveness probe is enabled
	podTemplateSpec.Spec.Containers[0] = opconfig.ConfigureLivenessProbe(cephv1.KeyOSD, podTemplateSpec.Spec.Containers[0], c.spec.HealthCheck)

	if c.spec.Network.IsHost() {
		podTemplateSpec.Spec.DNSPolicy = v1.DNSClusterFirstWithHostNet
	} else if c.spec.Network.NetworkSpec.IsMultus() {
		if err := k8sutil.ApplyMultus(c.spec.Network.NetworkSpec, &podTemplateSpec.ObjectMeta); err != nil {
			return nil, err
		}
	}

	deployment := &apps.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: c.clusterInfo.Namespace,
			Labels:    c.getOSDLabels(osd, failureDomainValue, osdProps.portable),
		},
		Spec: apps.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					k8sutil.AppAttr:     AppName,
					k8sutil.ClusterAttr: c.clusterInfo.Namespace,
					OsdIdLabelKey:       fmt.Sprintf("%d", osd.ID),
				},
			},
			Strategy: apps.DeploymentStrategy{
				Type: apps.RecreateDeploymentStrategyType,
			},
			Template: podTemplateSpec,
			Replicas: &replicaCount,
		},
	}
	if osdProps.onPVC() {
		k8sutil.AddLabelToDeployment(OSDOverPVCLabelKey, osdProps.pvc.ClaimName, deployment)
		k8sutil.AddLabelToPod(OSDOverPVCLabelKey, osdProps.pvc.ClaimName, &deployment.Spec.Template)
	}
	if !osdProps.portable {
		deployment.Spec.Template.Spec.NodeSelector = map[string]string{v1.LabelHostname: osdProps.crushHostname}
	}
	// Replace default unreachable node toleration if the osd pod is portable and based in PVC
	if osdProps.onPVC() && osdProps.portable {
		k8sutil.AddUnreachableNodeToleration(&deployment.Spec.Template.Spec)
	}

	k8sutil.AddRookVersionLabelToDeployment(deployment)
	cephv1.GetOSDAnnotations(c.spec.Annotations).ApplyToObjectMeta(&deployment.ObjectMeta)
	cephv1.GetOSDAnnotations(c.spec.Annotations).ApplyToObjectMeta(&deployment.Spec.Template.ObjectMeta)
	cephv1.GetOSDLabels(c.spec.Labels).ApplyToObjectMeta(&deployment.ObjectMeta)
	cephv1.GetOSDLabels(c.spec.Labels).ApplyToObjectMeta(&deployment.Spec.Template.ObjectMeta)
	controller.AddCephVersionLabelToDeployment(c.clusterInfo.CephVersion, deployment)
	controller.AddCephVersionLabelToDeployment(c.clusterInfo.CephVersion, deployment)
	k8sutil.SetOwnerRef(&deployment.ObjectMeta, &c.clusterInfo.OwnerRef)
	if !osdProps.onPVC() {
		cephv1.GetOSDPlacement(c.spec.Placement).ApplyToPodSpec(&deployment.Spec.Template.Spec)
	} else {
		osdProps.placement.ApplyToPodSpec(&deployment.Spec.Template.Spec)
	}

	return deployment, nil
}

// To get rook inside the container, the config init container needs to copy "tini" and "rook" binaries into a volume.
// Get the config flag so rook will copy the binaries and create the volume and mount that will be shared between
// the init container and the daemon container
func (c *Cluster) getCopyBinariesContainer() (v1.Volume, *v1.Container) {
	volume := v1.Volume{Name: rookBinariesVolumeName, VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}}
	mount := v1.VolumeMount{Name: rookBinariesVolumeName, MountPath: rookBinariesMountPath}

	return volume, &v1.Container{
		Args: []string{
			"copy-binaries",
			"--copy-to-dir", rookBinariesMountPath},
		Name:         "copy-bins",
		Image:        k8sutil.MakeRookImage(c.rookVersion),
		VolumeMounts: []v1.VolumeMount{mount},
	}
}

// This container runs all the actions needed to activate an OSD before we can run the OSD process
func (c *Cluster) getActivateOSDInitContainer(osdID string, osdInfo OSDInfo, osdProps osdProperties) (v1.Volume, *v1.Container) {
	volume := v1.Volume{Name: activateOSDVolumeName, VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}}
	envVars := osdActivateEnvVar()
	osdStore := "--bluestore"

	// Build empty dir osd path to something like "/var/lib/ceph/osd/ceph-0"
	activateOSDMountPathID := activateOSDMountPath + osdID

	volMounts := []v1.VolumeMount{
		{Name: activateOSDVolumeName, MountPath: activateOSDMountPathID},
		{Name: "devices", MountPath: "/dev"},
		{Name: k8sutil.ConfigOverrideName, ReadOnly: true, MountPath: opconfig.EtcCephDir},
	}

	if osdProps.onPVC() {
		volMounts = append(volMounts, getPvcOSDBridgeMount(osdProps.pvc.ClaimName))
	}

	container := &v1.Container{
		Command: []string{
			"/bin/bash",
			"-c",
			fmt.Sprintf(activateOSDCode, osdID, osdInfo.UUID, osdStore, osdInfo.CVMode, osdInfo.BlockPath, osdMetadataDeviceEnvVarName, osdWalDeviceEnvVarName),
		},
		Name:            "activate",
		Image:           c.spec.CephVersion.Image,
		VolumeMounts:    volMounts,
		SecurityContext: PrivilegedContext(),
		Env:             envVars,
		Resources:       osdProps.resources,
	}

	return volume, container
}

// Currently we can't mount a block mode pv directly to a privileged container
// So we mount it to a non privileged init container and then copy it to a common directory mounted inside init container
// and the privileged provision container.
func (c *Cluster) getPVCInitContainer(osdProps osdProperties) v1.Container {
	return v1.Container{
		Name:  blockPVCMapperInitContainer,
		Image: c.spec.CephVersion.Image,
		Command: []string{
			"cp",
		},
		Args: []string{"-a", fmt.Sprintf("/%s", osdProps.pvc.ClaimName), fmt.Sprintf("/mnt/%s", osdProps.pvc.ClaimName)},
		VolumeDevices: []v1.VolumeDevice{
			{
				Name:       osdProps.pvc.ClaimName,
				DevicePath: fmt.Sprintf("/%s", osdProps.pvc.ClaimName),
			},
		},
		VolumeMounts: []v1.VolumeMount{
			{
				MountPath: "/mnt",
				Name:      fmt.Sprintf("%s-bridge", osdProps.pvc.ClaimName),
			},
		},
		SecurityContext: opmon.PodSecurityContext(),
		Resources:       osdProps.resources,
	}
}

func (c *Cluster) getPVCInitContainerActivate(mountPath string, osdProps osdProperties) v1.Container {

	return v1.Container{
		Name:  blockPVCMapperInitContainer,
		Image: c.spec.CephVersion.Image,
		Command: []string{
			"cp",
		},
		Args: []string{"-a", fmt.Sprintf("/%s", osdProps.pvc.ClaimName), path.Join(mountPath, dmcryptBlockName)},
		VolumeDevices: []v1.VolumeDevice{
			{
				Name:       osdProps.pvc.ClaimName,
				DevicePath: fmt.Sprintf("/%s", osdProps.pvc.ClaimName),
			},
		},
		VolumeMounts:    []v1.VolumeMount{getPvcOSDBridgeMountActivate(mountPath, osdProps.pvc.ClaimName)},
		SecurityContext: opmon.PodSecurityContext(),
		Resources:       osdProps.resources,
	}
}

func (c *Cluster) generateEncryptionOpenBlockContainer(resources v1.ResourceRequirements, containerName, pvcName, blockType string) v1.Container {
	return v1.Container{
		Name:  containerName,
		Image: c.spec.CephVersion.Image,
		// Running via bash allows us to check whether the device is already opened or not
		// If we don't the cryptsetup command will fail saying the device is already opened
		Command: []string{
			"/bin/bash",
			"-c",
			fmt.Sprintf(openEncryptedBlock, encryptionKeyPath(), fmt.Sprintf("/%s", pvcName), encryptionDMName(pvcName, blockType), encryptionDMPath(pvcName, blockType)),
		},
		VolumeDevices: []v1.VolumeDevice{
			{
				Name:       pvcName,
				DevicePath: fmt.Sprintf("/%s", pvcName),
			},
		},
		VolumeMounts:    []v1.VolumeMount{getDeviceMapperMount()},
		SecurityContext: opmon.PodSecurityContext(),
		Resources:       resources,
	}
}

func (c *Cluster) getPVCEncryptionOpenInitContainerActivate(osdProps osdProperties) []v1.Container {
	containers := []v1.Container{}

	// Main block container
	blockContainer := c.generateEncryptionOpenBlockContainer(osdProps.resources, blockEncryptionOpenInitContainer, osdProps.pvc.ClaimName, DmcryptBlockType)
	_, volMount := getEncryptionVolume(osdProps.pvc.ClaimName)
	blockContainer.VolumeMounts = append(blockContainer.VolumeMounts, volMount)
	containers = append(containers, blockContainer)

	// If there is a metadata PVC
	if osdProps.metadataPVC.ClaimName != "" {
		metadataContainer := c.generateEncryptionOpenBlockContainer(osdProps.resources, blockEncryptionOpenMetadataInitContainer, osdProps.metadataPVC.ClaimName, DmcryptMetadataType)
		// We use the same key for both block and block.db so we must use osdProps.pvc.ClaimName for the getEncryptionVolume()
		_, volMount := getEncryptionVolume(osdProps.pvc.ClaimName)
		metadataContainer.VolumeMounts = append(metadataContainer.VolumeMounts, volMount)
		containers = append(containers, metadataContainer)
	}

	// If there is a wal PVC
	if osdProps.walPVC.ClaimName != "" {
		metadataContainer := c.generateEncryptionOpenBlockContainer(osdProps.resources, blockEncryptionOpenWalInitContainer, osdProps.walPVC.ClaimName, DmcryptWalType)
		// We use the same key for both block and block.db so we must use osdProps.pvc.ClaimName for the getEncryptionVolume()
		_, volMount := getEncryptionVolume(osdProps.pvc.ClaimName)
		metadataContainer.VolumeMounts = append(metadataContainer.VolumeMounts, volMount)
		containers = append(containers, metadataContainer)
	}

	return containers
}

func (c *Cluster) generateEncryptionCopyBlockContainer(resources v1.ResourceRequirements, containerName, pvcName, mountPath, volumeMountPVCName, blockName, blockType string) v1.Container {
	return v1.Container{
		Name:  containerName,
		Image: c.spec.CephVersion.Image,
		Command: []string{
			"cp",
		},
		Args: []string{"-a", encryptionDMPath(pvcName, blockType), path.Join(mountPath, blockName)},
		// volumeMountPVCName is crucial, especially when the block we copy is the metadata block
		// its value must be the name of the block PV so that all init containers use the same bridge (the emptyDir shared by all the init containers)
		VolumeMounts:    []v1.VolumeMount{getPvcOSDBridgeMountActivate(mountPath, volumeMountPVCName)},
		SecurityContext: opmon.PodSecurityContext(),
		Resources:       resources,
	}
}

func (c *Cluster) getPVCEncryptionInitContainerActivate(mountPath string, osdProps osdProperties) []v1.Container {
	containers := []v1.Container{}
	containers = append(containers, c.generateEncryptionCopyBlockContainer(osdProps.resources, blockPVCMapperEncryptionInitContainer, osdProps.pvc.ClaimName, mountPath, osdProps.pvc.ClaimName, dmcryptBlockName, DmcryptBlockType))

	// If there is a metadata PVC
	if osdProps.metadataPVC.ClaimName != "" {
		containers = append(containers, c.generateEncryptionCopyBlockContainer(osdProps.resources, blockPVCMapperEncryptionMetadataInitContainer, osdProps.metadataPVC.ClaimName, mountPath, osdProps.pvc.ClaimName, dmcryptMetadataName, DmcryptMetadataType))
	}

	// If there is a wal PVC
	if osdProps.walPVC.ClaimName != "" {
		containers = append(containers, c.generateEncryptionCopyBlockContainer(osdProps.resources, blockPVCMapperEncryptionWalInitContainer, osdProps.walPVC.ClaimName, mountPath, osdProps.pvc.ClaimName, dmcryptWalName, DmcryptWalType))
	}

	return containers
}

// The reason why this is not part of getPVCInitContainer is that this will change the deployment spec object
// and thus restart the osd deployment, so it is better to have it separated and only enable it
// It will change the deployment spec because we must add a new argument to the method like 'mountPath' and use it in the container name
// otherwise we will end up with a new conflict during the job/deployment initialization
func (c *Cluster) getPVCMetadataInitContainer(mountPath string, osdProps osdProperties) v1.Container {
	return v1.Container{
		Name:  blockPVCMetadataMapperInitContainer,
		Image: c.spec.CephVersion.Image,
		Command: []string{
			"cp",
		},
		Args: []string{"-a", fmt.Sprintf("/%s", osdProps.metadataPVC.ClaimName), fmt.Sprintf("/srv/%s", osdProps.metadataPVC.ClaimName)},
		VolumeDevices: []v1.VolumeDevice{
			{
				Name:       osdProps.metadataPVC.ClaimName,
				DevicePath: fmt.Sprintf("/%s", osdProps.metadataPVC.ClaimName),
			},
		},
		VolumeMounts: []v1.VolumeMount{
			{
				MountPath: "/srv",
				Name:      fmt.Sprintf("%s-bridge", osdProps.metadataPVC.ClaimName),
			},
		},
		SecurityContext: opmon.PodSecurityContext(),
		Resources:       osdProps.resources,
	}
}

func (c *Cluster) getPVCMetadataInitContainerActivate(mountPath string, osdProps osdProperties) v1.Container {
	return v1.Container{
		Name:  blockPVCMetadataMapperInitContainer,
		Image: c.spec.CephVersion.Image,
		Command: []string{
			"cp",
		},
		Args: []string{"-a", fmt.Sprintf("/%s", osdProps.metadataPVC.ClaimName), path.Join(mountPath, "block.db")},
		VolumeDevices: []v1.VolumeDevice{
			{
				Name:       osdProps.metadataPVC.ClaimName,
				DevicePath: fmt.Sprintf("/%s", osdProps.metadataPVC.ClaimName),
			},
		},
		// We need to call getPvcOSDBridgeMountActivate() so that we can copy the metadata block into the "main" empty dir
		// This empty dir is passed along every init container
		VolumeMounts:    []v1.VolumeMount{getPvcOSDBridgeMountActivate(mountPath, osdProps.pvc.ClaimName)},
		SecurityContext: opmon.PodSecurityContext(),
		Resources:       osdProps.resources,
	}
}

func (c *Cluster) getPVCWalInitContainer(mountPath string, osdProps osdProperties) v1.Container {
	return v1.Container{
		Name:  blockPVCWalMapperInitContainer,
		Image: c.spec.CephVersion.Image,
		Command: []string{
			"cp",
		},
		Args: []string{"-a", fmt.Sprintf("/%s", osdProps.walPVC.ClaimName), fmt.Sprintf("/wal/%s", osdProps.walPVC.ClaimName)},
		VolumeDevices: []v1.VolumeDevice{
			{
				Name:       osdProps.walPVC.ClaimName,
				DevicePath: fmt.Sprintf("/%s", osdProps.walPVC.ClaimName),
			},
		},
		VolumeMounts: []v1.VolumeMount{
			{
				MountPath: "/wal",
				Name:      fmt.Sprintf("%s-bridge", osdProps.walPVC.ClaimName),
			},
		},
		SecurityContext: opmon.PodSecurityContext(),
		Resources:       osdProps.resources,
	}
}

func (c *Cluster) getPVCWalInitContainerActivate(mountPath string, osdProps osdProperties) v1.Container {
	return v1.Container{
		Name:  blockPVCWalMapperInitContainer,
		Image: c.spec.CephVersion.Image,
		Command: []string{
			"cp",
		},
		Args: []string{"-a", fmt.Sprintf("/%s", osdProps.walPVC.ClaimName), path.Join(mountPath, "block.wal")},
		VolumeDevices: []v1.VolumeDevice{
			{
				Name:       osdProps.walPVC.ClaimName,
				DevicePath: fmt.Sprintf("/%s", osdProps.walPVC.ClaimName),
			},
		},
		// We need to call getPvcOSDBridgeMountActivate() so that we can copy the wal block into the "main" empty dir
		// This empty dir is passed along every init container
		VolumeMounts:    []v1.VolumeMount{getPvcOSDBridgeMountActivate(mountPath, osdProps.pvc.ClaimName)},
		SecurityContext: opmon.PodSecurityContext(),
		Resources:       osdProps.resources,
	}
}

func (c *Cluster) getActivatePVCInitContainer(osdProps osdProperties, osdID string) v1.Container {
	osdDataPath := activateOSDMountPath + osdID
	osdDataBlockPath := path.Join(osdDataPath, "block")

	container := v1.Container{
		Name:  activatePVCOSDInitContainer,
		Image: c.spec.CephVersion.Image,
		Command: []string{
			"ceph-bluestore-tool",
		},
		Args: []string{"prime-osd-dir", "--dev", osdDataBlockPath, "--path", osdDataPath, "--no-mon-config"},
		VolumeDevices: []v1.VolumeDevice{
			{
				Name:       osdProps.pvc.ClaimName,
				DevicePath: osdDataBlockPath,
			},
		},
		VolumeMounts:    []v1.VolumeMount{getPvcOSDBridgeMountActivate(osdDataPath, osdProps.pvc.ClaimName)},
		SecurityContext: PrivilegedContext(),
		Resources:       osdProps.resources,
	}

	return container
}

func (c *Cluster) getExpandPVCInitContainer(osdProps osdProperties, osdID string) v1.Container {
	/* Output example from 10GiB to 20GiB:

	   inferring bluefs devices from bluestore path
	   1 : device size 0x4ffe00000 : own 0x[11ff00000~40000000] = 0x40000000 : using 0x470000(4.4 MiB) : bluestore has 0x23fdd0000(9.0 GiB) available
	   Expanding DB/WAL...
	   Expanding Main...
	   1 : expanding  from 0x27fe00000 to 0x4ffe00000
	   1 : size label updated to 21472739328

	*/
	osdDataPath := activateOSDMountPath + osdID

	return v1.Container{
		Name:  expandPVCOSDInitContainer,
		Image: c.spec.CephVersion.Image,
		Command: []string{
			"ceph-bluestore-tool",
		},
		Args:            []string{"bluefs-bdev-expand", "--path", osdDataPath},
		VolumeMounts:    []v1.VolumeMount{getPvcOSDBridgeMountActivate(osdDataPath, osdProps.pvc.ClaimName)},
		SecurityContext: PrivilegedContext(),
		Resources:       osdProps.resources,
	}
}

func (c *Cluster) getExpandEncryptedPVCInitContainer(mountPath string, osdProps osdProperties) v1.Container {
	/* Command example
	   [root@rook-ceph-osd-0-59b9947547-w8mdq /]# cryptsetup resize set1-data-2-8n462-block-dmcrypt
	   Command successful.
	*/

	// Add /dev/mapper in the volume mount list
	// This will fix issues when running on multi-path, where cryptsetup complains that the underlying device does not exist
	// Essentially, the device cannot be found because it was not mounted in the container
	// Typically, the device is mapped to the OSD data dir so it is mounted
	volMount := []v1.VolumeMount{getPvcOSDBridgeMountActivate(mountPath, osdProps.pvc.ClaimName)}
	_, volMountMapper := getDeviceMapperVolume()
	volMount = append(volMount, volMountMapper)

	return v1.Container{
		Name:  expandEncryptedPVCOSDInitContainer,
		Image: c.spec.CephVersion.Image,
		Command: []string{
			"cryptsetup",
		},
		Args:            []string{"--verbose", "resize", encryptionDMName(osdProps.pvc.ClaimName, DmcryptBlockType)},
		VolumeMounts:    volMount,
		SecurityContext: PrivilegedContext(),
		Resources:       osdProps.resources,
	}
}

func (c *Cluster) getEncryptedStatusPVCInitContainer(mountPath string, osdProps osdProperties) v1.Container {
	/* Command example:
		root@rook-ceph-osd-0-59b9947547-w8mdq /]# cryptsetup status set1-data-2-8n462-block-dmcrypt -v
	   /dev/mapper/set1-data-2-8n462-block-dmcrypt is active and is in use.
	     type:    LUKS1
	     cipher:  aes-xts-plain64
	     keysize: 256 bits
	     key location: dm-crypt
	     device:  /dev/xvdbv
	     sector size:  512
	     offset:  4096 sectors
	     size:    20967424 sectors
	     mode:    read/write
	     flags:   discards
	   Command successful.
	*/

	return v1.Container{
		Name:  encryptedPVCStatusOSDInitContainer,
		Image: c.spec.CephVersion.Image,
		Command: []string{
			"cryptsetup",
		},
		Args:            []string{"--verbose", "status", encryptionDMName(osdProps.pvc.ClaimName, DmcryptBlockType)},
		VolumeMounts:    []v1.VolumeMount{getPvcOSDBridgeMountActivate(mountPath, osdProps.pvc.ClaimName)},
		SecurityContext: PrivilegedContext(),
		Resources:       osdProps.resources,
	}
}
