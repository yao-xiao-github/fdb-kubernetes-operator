/*
 * replace_failed_process_groups.go
 *
 * This source file is part of the FoundationDB open source project
 *
 * Copyright 2021 Apple Inc. and the FoundationDB project authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package replacements

import (
	"fmt"
	"time"

	fdbv1beta2 "github.com/FoundationDB/fdb-kubernetes-operator/api/v1beta2"
	"github.com/FoundationDB/fdb-kubernetes-operator/internal"
	"github.com/FoundationDB/fdb-kubernetes-operator/pkg/fdbadminclient"
	"github.com/go-logr/logr"
)

func getMaxReplacements(cluster *fdbv1beta2.FoundationDBCluster, maxReplacements int) int {
	// The maximum number of replacements will be the defined number in the cluster spec
	// minus all currently ongoing replacements e.g. process groups marked for removal but
	// not fully excluded.
	removalCount := 0
	for _, processGroupStatus := range cluster.Status.ProcessGroups {
		if processGroupStatus.IsMarkedForRemoval() && !processGroupStatus.IsExcluded() {
			// If we already have a removal in-flight, we should not try
			// replacing more failed process groups.
			removalCount++
		}
	}

	return maxReplacements - removalCount
}

// ReplaceFailedProcessGroups flags failed processes groups for removal and returns an indicator
// of whether any processes were thus flagged.
func ReplaceFailedProcessGroups(log logr.Logger, cluster *fdbv1beta2.FoundationDBCluster, adminClient fdbadminclient.AdminClient) bool {
	// Automatic replacements are disabled, so we don't have to check anything further
	if !cluster.GetEnableAutomaticReplacements() {
		return false
	}

	crashLoopProcessGroups, crashLoopAll := cluster.GetCrashLoopProcessGroups()
	// If all process groups are in crash loop don't replace any process group.
	if crashLoopAll {
		return false
	}

	maxReplacements := getMaxReplacements(cluster, cluster.GetMaxConcurrentAutomaticReplacements())
	hasReplacement := false
	for _, processGroupStatus := range cluster.Status.ProcessGroups {
		if maxReplacements <= 0 {
			return hasReplacement
		}

		// Don't replace processes that are in the crash loop setting. Otherwise, we might replace process groups that
		// are in that state for debugging or stability.
		if _, ok := crashLoopProcessGroups[processGroupStatus.ProcessGroupID]; ok {
			continue
		}

		needsReplacement, missingTime := processGroupStatus.NeedsReplacement(cluster.GetFailureDetectionTimeSeconds())
		if !needsReplacement {
			continue
		}

		if len(processGroupStatus.Addresses) == 0 {
			// Only replace process groups without an address if the cluster has the desired fault tolerance
			// and is available.
			hasDesiredFaultTolerance, err := internal.HasDesiredFaultTolerance(log, adminClient, cluster)
			if err != nil {
				log.Error(err, "Could not fetch if cluster has desired fault tolerance")
				continue
			}

			if !hasDesiredFaultTolerance {
				log.Info(
					"Skip process group with missing address",
					"processGroupID", processGroupStatus.ProcessGroupID,
					"failureTime", time.Unix(missingTime, 0).UTC().String())
				continue
			}

			// Since the process groups doesn't contain any addresses we have to skip exclusion.
			// The assumption here is that this is safe since we assume that the process group was never scheduled onto any node
			// otherwise the process group should have an address associated.
			processGroupStatus.ExclusionSkipped = true
			log.Info(
				"Replace process group with missing address",
				"processGroupID", processGroupStatus.ProcessGroupID,
				"failureTime", time.Unix(missingTime, 0).UTC().String())
		}

		log.Info("Replace process group",
			"processGroupID", processGroupStatus.ProcessGroupID,
			"reason", fmt.Sprintf("automatic replacement detected failure time: %s", time.Unix(missingTime, 0).UTC().String()))

		processGroupStatus.MarkForRemoval()
		hasReplacement = true
		maxReplacements--
	}

	return hasReplacement
}
