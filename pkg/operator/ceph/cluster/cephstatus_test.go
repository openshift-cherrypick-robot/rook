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
	"reflect"
	"testing"
	"time"

	"github.com/pkg/errors"
	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/daemon/ceph/client"
	cephclient "github.com/rook/rook/pkg/daemon/ceph/client"
	exectest "github.com/rook/rook/pkg/util/exec/test"
	"github.com/stretchr/testify/assert"
	controllerclient "sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCephStatus(t *testing.T) {
	newStatus := &cephclient.CephStatus{
		Health: cephclient.HealthStatus{Status: "HEALTH_OK"},
	}

	// Empty initial status will have no previous health
	currentStatus := cephv1.ClusterStatus{}
	aggregateStatus := toCustomResourceStatus(currentStatus, newStatus)
	assert.NotNil(t, aggregateStatus)
	assert.Equal(t, "HEALTH_OK", aggregateStatus.Health)
	assert.NotEqual(t, "", aggregateStatus.LastChecked)
	assert.Equal(t, "", aggregateStatus.LastChanged)
	assert.Equal(t, "", aggregateStatus.PreviousHealth)
	assert.Equal(t, 0, len(aggregateStatus.Details))

	// Set the current status to the same as the new status and there will be no previous health
	currentStatus.CephStatus = &cephv1.CephStatus{
		Health: "HEALTH_OK",
	}
	aggregateStatus = toCustomResourceStatus(currentStatus, newStatus)
	assert.NotNil(t, aggregateStatus)
	assert.Equal(t, "HEALTH_OK", aggregateStatus.Health)
	assert.NotEqual(t, "", aggregateStatus.LastChecked)
	assert.Equal(t, "", aggregateStatus.LastChanged)
	assert.Equal(t, "", aggregateStatus.PreviousHealth)
	assert.Equal(t, 0, len(aggregateStatus.Details))

	// Set the new status to something different and we should get a previous health
	// Simulate the previous check a minute ago.
	previousTime := formatTime(time.Now().Add(-time.Minute).UTC())
	currentStatus.CephStatus.LastChecked = previousTime
	newStatus.Health.Status = "HEALTH_WARN"
	aggregateStatus = toCustomResourceStatus(currentStatus, newStatus)
	assert.NotNil(t, aggregateStatus)
	assert.Equal(t, "HEALTH_WARN", aggregateStatus.Health)
	assert.NotEqual(t, "", aggregateStatus.LastChecked)
	assert.Equal(t, aggregateStatus.LastChecked, aggregateStatus.LastChanged)
	assert.Equal(t, "HEALTH_OK", aggregateStatus.PreviousHealth)
	assert.Equal(t, 0, len(aggregateStatus.Details))

	// Add some details to the warning
	osdDownMsg := cephclient.CheckMessage{Severity: "HEALTH_WARN"}
	osdDownMsg.Summary.Message = "1 osd down"
	pgAvailMsg := cephclient.CheckMessage{Severity: "HEALTH_ERR"}
	pgAvailMsg.Summary.Message = "'Reduced data availability: 100 pgs stale'"
	newStatus.Health.Checks = map[string]cephclient.CheckMessage{
		"OSD_DOWN":        osdDownMsg,
		"PG_AVAILABILITY": pgAvailMsg,
	}
	newStatus.Health.Status = "HEALTH_ERR"
	aggregateStatus = toCustomResourceStatus(currentStatus, newStatus)
	assert.NotNil(t, aggregateStatus)
	assert.Equal(t, "HEALTH_ERR", aggregateStatus.Health)
	assert.NotEqual(t, "", aggregateStatus.LastChecked)
	assert.Equal(t, aggregateStatus.LastChecked, aggregateStatus.LastChanged)
	assert.Equal(t, "HEALTH_OK", aggregateStatus.PreviousHealth)
	assert.Equal(t, 2, len(aggregateStatus.Details))
	assert.Equal(t, osdDownMsg.Summary.Message, aggregateStatus.Details["OSD_DOWN"].Message)
	assert.Equal(t, osdDownMsg.Severity, aggregateStatus.Details["OSD_DOWN"].Severity)
	assert.Equal(t, pgAvailMsg.Summary.Message, aggregateStatus.Details["PG_AVAILABILITY"].Message)
	assert.Equal(t, pgAvailMsg.Severity, aggregateStatus.Details["PG_AVAILABILITY"].Severity)
}

func TestNewCephStatusChecker(t *testing.T) {
	clusterInfo := client.AdminClusterInfo("ns")
	c := &clusterd.Context{}
	time10s, err := time.ParseDuration("10s")
	assert.NoError(t, err)

	type args struct {
		context     *clusterd.Context
		clusterInfo *cephclient.ClusterInfo
		clusterSpec *cephv1.ClusterSpec
	}
	tests := []struct {
		name string
		args args
		want *cephStatusChecker
	}{
		{"default-interval", args{c, clusterInfo, &cephv1.ClusterSpec{}}, &cephStatusChecker{c, clusterInfo, defaultStatusCheckInterval, c.Client, false}},
		{"10s-interval", args{c, clusterInfo, &cephv1.ClusterSpec{HealthCheck: cephv1.CephClusterHealthCheckSpec{DaemonHealth: cephv1.DaemonHealthSpec{Status: cephv1.HealthCheckSpec{Interval: "10s"}}}}}, &cephStatusChecker{c, clusterInfo, time10s, c.Client, false}},
		{"10s-interval-external", args{c, clusterInfo, &cephv1.ClusterSpec{External: cephv1.ExternalSpec{Enable: true}, HealthCheck: cephv1.CephClusterHealthCheckSpec{DaemonHealth: cephv1.DaemonHealthSpec{Status: cephv1.HealthCheckSpec{Interval: "10s"}}}}}, &cephStatusChecker{c, clusterInfo, time10s, c.Client, true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := newCephStatusChecker(tt.args.context, tt.args.clusterInfo, tt.args.clusterSpec); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("newCephStatusChecker() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_cephStatusChecker_conditionMessageReason(t *testing.T) {
	c := &clusterd.Context{}
	clusterInfo := client.AdminClusterInfo("ns")
	type fields struct {
		context     *clusterd.Context
		clusterInfo *cephclient.ClusterInfo
		interval    time.Duration
		client      controllerclient.Client
		isExternal  bool
	}
	type args struct {
		condition cephv1.ConditionType
	}
	tests := []struct {
		name   string
		fields fields
		args   args
		want   cephv1.ConditionType
		want1  string
		want2  string
	}{
		{"failure-converged", fields{c, clusterInfo, defaultStatusCheckInterval, c.Client, false}, args{cephv1.ConditionFailure}, cephv1.ConditionFailure, "ClusterFailure", "Failed to configure ceph cluster"},
		{"failure-external", fields{c, clusterInfo, defaultStatusCheckInterval, c.Client, true}, args{cephv1.ConditionFailure}, cephv1.ConditionFailure, "ClusterFailure", "Failed to configure external ceph cluster"},
		{"success-converged", fields{c, clusterInfo, defaultStatusCheckInterval, c.Client, false}, args{cephv1.ConditionReady}, cephv1.ConditionReady, "ClusterCreated", "Cluster created successfully"},
		{"success-external", fields{c, clusterInfo, defaultStatusCheckInterval, c.Client, true}, args{cephv1.ConditionReady}, cephv1.ConditionConnected, "ClusterConnected", "Cluster connected successfully"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &cephStatusChecker{
				context:     tt.fields.context,
				clusterInfo: tt.fields.clusterInfo,
				interval:    tt.fields.interval,
				client:      tt.fields.client,
				isExternal:  tt.fields.isExternal,
			}
			got, got1, got2 := c.conditionMessageReason(tt.args.condition)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("cephStatusChecker.conditionMessageReason() got = %v, want %v", got, tt.want)
			}
			if got1 != tt.want1 {
				t.Errorf("cephStatusChecker.conditionMessageReason() got1 = %v, want %v", got1, tt.want1)
			}
			if got2 != tt.want2 {
				t.Errorf("cephStatusChecker.conditionMessageReason() got2 = %v, want %v", got2, tt.want2)
			}

		})
	}
}

func TestConfigureHealthSettings(t *testing.T) {
	c := &cephStatusChecker{
		context:     &clusterd.Context{},
		clusterInfo: cephclient.AdminClusterInfo("ns"),
	}
	getGlobalIDReclaim := false
	setGlobalIDReclaim := false
	c.context.Executor = &exectest.MockExecutor{
		MockExecuteCommandWithOutputFile: func(command, outfile string, args ...string) (string, error) {
			logger.Infof("Command: %s %v", command, args)
			if args[0] == "config" && args[3] == "auth_allow_insecure_global_id_reclaim" {
				if args[1] == "get" {
					getGlobalIDReclaim = true
					return "", nil
				}
				if args[1] == "set" {
					setGlobalIDReclaim = true
					return "", nil
				}
			}
			return "", errors.New("mock error to simulate failure of SetConfig() function")
		},
	}
	noActionOneWarningStatus := cephclient.CephStatus{
		Health: cephclient.HealthStatus{
			Checks: map[string]cephclient.CheckMessage{
				"MDS_ALL_DOWN": {
					Severity: "HEALTH_WARN",
					Summary: cephclient.Summary{
						Message: "MDS_ALL_DOWN",
					},
				},
			},
		},
	}
	disableInsecureGlobalIDStatus := cephclient.CephStatus{
		Health: cephclient.HealthStatus{
			Checks: map[string]cephclient.CheckMessage{
				"AUTH_INSECURE_GLOBAL_ID_RECLAIM_ALLOWED": {
					Severity: "HEALTH_WARN",
					Summary: cephclient.Summary{
						Message: "foo",
					},
				},
			},
		},
	}
	noDisableInsecureGlobalIDStatus := cephclient.CephStatus{
		Health: cephclient.HealthStatus{
			Checks: map[string]cephclient.CheckMessage{
				"AUTH_INSECURE_GLOBAL_ID_RECLAIM_ALLOWED": {
					Severity: "HEALTH_WARN",
					Summary: cephclient.Summary{
						Message: "foo",
					},
				},
				"AUTH_INSECURE_GLOBAL_ID_RECLAIM": {
					Severity: "HEALTH_WARN",
					Summary: cephclient.Summary{
						Message: "bar",
					},
				},
			},
		},
	}

	type args struct {
		status                     cephclient.CephStatus
		expectedGetGlobalIDSetting bool
		expectedSetGlobalIDSetting bool
	}
	tests := []struct {
		name string
		args args
	}{
		{"no-warnings", args{cephclient.CephStatus{}, false, false}},
		{"no-action-one-warning", args{noActionOneWarningStatus, false, false}},
		{"disable-insecure-global-id", args{disableInsecureGlobalIDStatus, true, true}},
		{"no-disable-insecure-global-id", args{noDisableInsecureGlobalIDStatus, false, false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getGlobalIDReclaim = false
			setGlobalIDReclaim = false
			c.configureHealthSettings(tt.args.status)
			assert.Equal(t, tt.args.expectedGetGlobalIDSetting, getGlobalIDReclaim)
			assert.Equal(t, tt.args.expectedSetGlobalIDSetting, setGlobalIDReclaim)
		})
	}
}
