/*
This file is part of Cloud Native PostgreSQL.

Copyright (C) 2019-2021 EnterpriseDB Corporation.
*/

// Package walarchive implement the wal-archive command
package walarchive

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/blang/semver"
	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/EnterpriseDB/cloud-native-postgresql/api/v1"
	"github.com/EnterpriseDB/cloud-native-postgresql/internal/management/cache"
	cacheClient "github.com/EnterpriseDB/cloud-native-postgresql/internal/management/cache/client"
	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/management"
	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/management/barman"
	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/management/execlog"
	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/management/log"
)

const barmanCloudWalArchiveName = "barman-cloud-wal-archive"

// NewCmd creates the new cobra command
func NewCmd() *cobra.Command {
	var clusterName string
	var namespace string

	cmd := cobra.Command{
		Use:           "wal-archive [name]",
		SilenceErrors: true,
		Args:          cobra.ExactArgs(1),
		RunE: func(cobraCmd *cobra.Command, args []string) error {
			contextLog := log.WithName("wal-archive")
			err := run(contextLog, namespace, clusterName, args)
			if err != nil {
				contextLog.Error(err, "failed to run wal-archive command")
				return err
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&clusterName, "cluster-name", os.Getenv("CLUSTER_NAME"), "The name of the "+
		"current cluster in k8s")
	cmd.Flags().StringVar(&namespace, "namespace", os.Getenv("NAMESPACE"), "The namespace of "+
		"the cluster and of the Pod in k8s")

	return &cmd
}

func run(contextLog log.Logger, namespace, clusterName string, args []string) error {
	ctx := context.Background()

	walName := args[0]

	var cluster *apiv1.Cluster
	var err error
	var typedClient client.Client

	typedClient, err = management.NewControllerRuntimeClient()
	if err != nil {
		contextLog.Error(err, "Error while creating k8s client")
		return err
	}

	cluster, err = cacheClient.GetCluster(ctx, typedClient, namespace, clusterName)
	if err != nil {
		contextLog.Error(err, "Error while getting cluster from cache")
		return fmt.Errorf("failed to get cluster: %w", err)
	}

	if cluster.Spec.Backup == nil || cluster.Spec.Backup.BarmanObjectStore == nil {
		// Backup not configured, skipping WAL
		contextLog.Info("Backup not configured, skip WAL archiving",
			"walName", walName,
			"currentPrimary", cluster.Status.CurrentPrimary,
			"targetPrimary", cluster.Status.TargetPrimary,
		)
		return nil
	}
	version, err := barman.GetBarmanCloudVersion(barmanCloudWalArchiveName)
	if err != nil {
		contextLog.Error(err, "while getting barman-cloud-wal-archive version")
	}

	options, err := barmanCloudWalArchiveOptions(*cluster, clusterName, walName, version)
	if err != nil {
		contextLog.Error(err, "while getting barman-cloud-wal-archive options")
		return err
	}

	env, err := cacheClient.GetEnv(ctx,
		typedClient,
		cluster.Namespace,
		cluster.Spec.Backup.BarmanObjectStore,
		cache.WALArchiveKey)
	if err != nil {
		contextLog.Error(err, "Error while getting environment from cache")
		return fmt.Errorf("failed to get envs: %w", err)
	}

	contextLog.Trace("Executing "+barmanCloudWalArchiveName,
		"walName", walName,
		"currentPrimary", cluster.Status.CurrentPrimary,
		"targetPrimary", cluster.Status.TargetPrimary,
		"options", options,
	)

	barmanCloudWalArchiveCmd := exec.Command(barmanCloudWalArchiveName, options...) // #nosec G204
	barmanCloudWalArchiveCmd.Env = env

	err = execlog.RunStreaming(barmanCloudWalArchiveCmd, barmanCloudWalArchiveName)
	if err != nil {
		contextLog.Error(err, "Error invoking "+barmanCloudWalArchiveName,
			"walName", walName,
			"currentPrimary", cluster.Status.CurrentPrimary,
			"targetPrimary", cluster.Status.TargetPrimary,
			"options", options,
			"exitCode", barmanCloudWalArchiveCmd.ProcessState.ExitCode(),
		)
		return fmt.Errorf("unexpected failure invoking %s: %w", barmanCloudWalArchiveName, err)
	}

	contextLog.Info("Archived WAL file",
		"walName", walName,
		"currentPrimary", cluster.Status.CurrentPrimary,
		"targetPrimary", cluster.Status.TargetPrimary,
	)

	return nil
}

func barmanCloudWalArchiveOptions(
	cluster apiv1.Cluster,
	clusterName string,
	walName string,
	version *semver.Version,
) ([]string, error) {
	var barmanCloudVersionGE213 bool
	if version != nil {
		barmanCloudVersionGE213 = version.GE(semver.Version{Major: 2, Minor: 13})
	}
	configuration := cluster.Spec.Backup.BarmanObjectStore

	var options []string
	if configuration.Wal != nil {
		if len(configuration.Wal.Compression) != 0 {
			options = append(
				options,
				fmt.Sprintf("--%v", configuration.Wal.Compression))
		}
		if len(configuration.Wal.Encryption) != 0 {
			options = append(
				options,
				"-e",
				string(configuration.Wal.Encryption))
		}
	}
	if len(configuration.EndpointURL) > 0 {
		options = append(
			options,
			"--endpoint-url",
			configuration.EndpointURL)
	}

	if barmanCloudVersionGE213 {
		if configuration.S3Credentials != nil {
			options = append(
				options,
				"--cloud-provider",
				"aws-s3")
		}
		if configuration.AzureCredentials != nil {
			options = append(
				options,
				"--cloud-provider",
				"azure-blob-storage")
		}
	} else if configuration.AzureCredentials != nil {
		return nil, fmt.Errorf("barman >= 2.13 is required to use Azure object storage, current: %v", version)
	}

	serverName := clusterName
	if len(configuration.ServerName) != 0 {
		serverName = configuration.ServerName
	}
	options = append(
		options,
		configuration.DestinationPath,
		serverName,
		walName)
	return options, nil
}
