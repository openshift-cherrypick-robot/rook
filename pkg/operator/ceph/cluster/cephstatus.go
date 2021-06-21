/*
Copyright 2019 The Rook Authors. All rights reserved.

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

// Package cluster to manage Kubernetes storage.
package cluster

import (
	"os"
	"time"

	"github.com/pkg/errors"
	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	"github.com/rook/rook/pkg/clusterd"
	cephclient "github.com/rook/rook/pkg/daemon/ceph/client"
	"github.com/rook/rook/pkg/operator/ceph/config"
	opcontroller "github.com/rook/rook/pkg/operator/ceph/controller"
	cephver "github.com/rook/rook/pkg/operator/ceph/version"
	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// defaultStatusCheckInterval is the interval to check the status of the ceph cluster
	defaultStatusCheckInterval = 60 * time.Second
)

// cephStatusChecker aggregates the mon/cluster info needed to check the health of the monitors
type cephStatusChecker struct {
	context     *clusterd.Context
	clusterInfo *cephclient.ClusterInfo
	interval    time.Duration
	client      client.Client
	isExternal  bool
}

// newCephStatusChecker creates a new HealthChecker object
func newCephStatusChecker(context *clusterd.Context, clusterInfo *cephclient.ClusterInfo, clusterSpec *cephv1.ClusterSpec) *cephStatusChecker {
	c := &cephStatusChecker{
		context:     context,
		clusterInfo: clusterInfo,
		interval:    defaultStatusCheckInterval,
		client:      context.Client,
		isExternal:  clusterSpec.External.Enable,
	}

	// allow overriding the check interval with an env var on the operator
	// Keep the existing behavior
	var checkInterval string
	checkIntervalCRSetting := clusterSpec.HealthCheck.DaemonHealth.Status.Interval
	checkIntervalEnv := os.Getenv("ROOK_CEPH_STATUS_CHECK_INTERVAL")
	if checkIntervalEnv != "" {
		checkInterval = checkIntervalEnv
	}

	if checkIntervalCRSetting != "" && checkIntervalEnv == "" {
		checkInterval = checkIntervalCRSetting
	}

	// Set duration
	if checkInterval != "" {
		if duration, err := time.ParseDuration(checkInterval); err == nil {
			logger.Infof("ceph status check interval is %s", checkInterval)
			c.interval = duration
		}
	}

	return c
}

// checkCephStatus periodically checks the health of the cluster
func (c *cephStatusChecker) checkCephStatus(stopCh chan struct{}) {
	// check the status immediately before starting the loop
	c.checkStatus()

	for {
		select {
		case <-stopCh:
			logger.Infof("stopping monitoring of ceph status")
			return

		case <-time.After(c.interval):
			c.checkStatus()
		}
	}
}

// checkStatus queries the status of ceph health then updates the CR status
func (c *cephStatusChecker) checkStatus() {
	var status cephclient.CephStatus
	var err error

	logger.Debugf("checking health of cluster")

	// Check ceph's status
	status, err = cephclient.StatusWithUser(c.context, c.clusterInfo)
	if err != nil {
		logger.Errorf("failed to get ceph status. %v", err)
		condition, reason, message := c.conditionMessageReason(cephv1.ConditionFailure)
		if err := c.updateCephStatus(cephStatusOnError(err.Error()), condition, reason, message); err != nil {
			logger.Errorf("failed to query cluster status in namespace %q. %v", c.clusterInfo.Namespace, err)
		}
		return
	}

	logger.Debugf("cluster status: %+v", status)
	condition, reason, message := c.conditionMessageReason(cephv1.ConditionReady)
	if err := c.updateCephStatus(&status, condition, reason, message); err != nil {
		logger.Errorf("failed to query cluster status in namespace %q. %v", c.clusterInfo.Namespace, err)
	}

	c.configureHealthSettings(status)
}

func (c *cephStatusChecker) configureHealthSettings(status cephclient.CephStatus) {
	// loop through the health codes and log what we find
	for healthCode, check := range status.Health.Checks {
		logger.Debugf("Health: %q, code: %q, message: %q", check.Severity, healthCode, check.Summary.Message)
	}

	// disable the insecure global id if there are no old clients
	if _, ok := status.Health.Checks["AUTH_INSECURE_GLOBAL_ID_RECLAIM_ALLOWED"]; ok {
		if _, ok := status.Health.Checks["AUTH_INSECURE_GLOBAL_ID_RECLAIM"]; !ok {
			logger.Info("Disabling the insecure global ID as no legacy clients are currently connected. If you still require the insecure connections, see the CVE to suppress the health warning and re-enable the insecure connections. https://docs.ceph.com/en/latest/security/CVE-2021-20288/")
			if _, err := cephclient.SetConfig(c.context, c.clusterInfo, "mon", "auth_allow_insecure_global_id_reclaim", "false", false); err != nil {
				logger.Warningf("failed to disable the insecure global ID. %v", err)
			} else {
				logger.Info("insecure global ID is now disabled")
			}
		} else {
			logger.Warning("insecure clients are connected to the cluster, to resolve the AUTH_INSECURE_GLOBAL_ID_RECLAIM health warning please refer to the upgrade guide to ensure all Ceph daemons are updated.")
		}
	}
}

// updateStatus updates an object with a given status
func (c *cephStatusChecker) updateCephStatus(status *cephclient.CephStatus, condition cephv1.ConditionType, reason, message string) error {
	clusterName := c.clusterInfo.NamespacedName()
	cephCluster, err := c.context.RookClientset.CephV1().CephClusters(clusterName.Namespace).Get(clusterName.Name, metav1.GetOptions{})
	if err != nil {
		if kerrors.IsNotFound(err) {
			logger.Debug("CephCluster resource not found. Ignoring since object must be deleted.")
			return nil
		}
		return errors.Wrapf(err, "failed to retrieve ceph cluster %q in namespace %q to update status to %+v", clusterName.Name, clusterName.Namespace, status)
	}

	// Update with Ceph Status
	cephCluster.Status.CephStatus = toCustomResourceStatus(cephCluster.Status, status)
	cephCluster.Status.Phase = condition
	if err := opcontroller.UpdateStatus(c.client, cephCluster); err != nil {
		return errors.Wrapf(err, "failed to update cluster %q status", clusterName.Namespace)
	}

	// Update condition
	config.ConditionExport(c.context, c.clusterInfo.NamespacedName(), condition, v1.ConditionTrue, reason, message)

	logger.Debugf("ceph cluster %q status and condition updated to %+v, %v, %s, %s", clusterName.Namespace, status, v1.ConditionTrue, reason, message)
	return nil
}

// toCustomResourceStatus converts the ceph status to the struct expected for the CephCluster CR status
func toCustomResourceStatus(currentStatus cephv1.ClusterStatus, newStatus *cephclient.CephStatus) *cephv1.CephStatus {
	s := &cephv1.CephStatus{
		Health:      newStatus.Health.Status,
		LastChecked: formatTime(time.Now().UTC()),
		Details:     make(map[string]cephv1.CephHealthMessage),
	}
	for name, message := range newStatus.Health.Checks {
		s.Details[name] = cephv1.CephHealthMessage{
			Severity: message.Severity,
			Message:  message.Summary.Message,
		}
	}
	if currentStatus.CephStatus != nil {
		s.PreviousHealth = currentStatus.CephStatus.PreviousHealth
		s.LastChanged = currentStatus.CephStatus.LastChanged
		if currentStatus.CephStatus.Health != s.Health {
			s.PreviousHealth = currentStatus.CephStatus.Health
			s.LastChanged = s.LastChecked
		}
	}
	return s
}

func formatTime(t time.Time) string {
	return t.Format(time.RFC3339)
}

func (c *ClusterController) updateClusterCephVersion(image string, cephVersion cephver.CephVersion) {
	logger.Infof("cluster %q: version %q detected for image %q", c.namespacedName.Namespace, cephVersion.String(), image)

	cephCluster, err := c.context.RookClientset.CephV1().CephClusters(c.namespacedName.Namespace).Get(c.namespacedName.Name, metav1.GetOptions{})
	if err != nil {
		if kerrors.IsNotFound(err) {
			logger.Debug("CephCluster resource not found. Ignoring since object must be deleted.")
			return
		}
		logger.Errorf("failed to retrieve ceph cluster %q to update ceph version to %+v. %v", c.namespacedName.Name, cephVersion, err)
		return
	}

	cephClusterVersion := &cephv1.ClusterVersion{
		Image:   image,
		Version: opcontroller.GetCephVersionLabel(cephVersion),
	}
	// update the Ceph version on the retrieved cluster object
	// do not overwrite the ceph status that is updated in a separate goroutine
	cephCluster.Status.CephVersion = cephClusterVersion
	if err := opcontroller.UpdateStatus(c.client, cephCluster); err != nil {
		logger.Errorf("failed to update cluster %q version. %v", c.namespacedName.Name, err)
		return
	}
}

func cephStatusOnError(errorMessage string) *cephclient.CephStatus {
	details := make(map[string]cephclient.CheckMessage)
	details["error"] = cephclient.CheckMessage{
		Severity: "Urgent",
		Summary: cephclient.Summary{
			Message: errorMessage,
		},
	}

	return &cephclient.CephStatus{
		Health: cephclient.HealthStatus{
			Status: "HEALTH_ERR",
			Checks: details,
		},
	}
}

func (c *cephStatusChecker) conditionMessageReason(condition cephv1.ConditionType) (cephv1.ConditionType, string, string) {
	var reason, message string

	switch condition {
	case cephv1.ConditionFailure:
		reason = "ClusterFailure"
		message = "Failed to configure ceph cluster"
		if c.isExternal {
			message = "Failed to configure external ceph cluster"
		}
	case cephv1.ConditionReady:
		reason = "ClusterCreated"
		message = "Cluster created successfully"
		if c.isExternal {
			condition = cephv1.ConditionConnected
			reason = "ClusterConnected"
			message = "Cluster connected successfully"
		}
	}

	return condition, reason, message
}
