package handler

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openshift-online/ocm-common/pkg/test/kms_key"
	"github.com/openshift-online/ocm-common/pkg/test/vpc_client"

	"github.com/openshift/rosa/pkg/ocm"
	"github.com/openshift/rosa/tests/ci/config"
	ClusterConfigure "github.com/openshift/rosa/tests/utils/config"
	"github.com/openshift/rosa/tests/utils/constants"
	"github.com/openshift/rosa/tests/utils/exec/rosacli"
	"github.com/openshift/rosa/tests/utils/helper"
	"github.com/openshift/rosa/tests/utils/log"
)

const envVariableErrMsg = "'SHARED_VPC_AWS_SHARED_CREDENTIALS_FILE' env is not set or empty, it is: %s"

type ClusterHandler interface {
	GenerateClusterCreateFlags() ([]string, error)
	CreateCluster(waitForClusterReady bool) error
	WaitForClusterReady(timeoutMin int) error
	Destroy() []error
	GetClusterDetail() *ClusterDetail
	GetResourcesHandler() ResourcesHandler
}

type clusterHandler struct {
	profile          *Profile
	clusterDetail    *ClusterDetail
	clusterConfig    *ClusterConfigure.ClusterConfig
	resourcesHandler *resourcesHandler

	rosaClient *rosacli.Client
}

// NewClusterHandler create a new cluster handler with data persisted to Filesystem
// Need to call `saveToFile` method to make sure it persists all information
func NewClusterHandler(client *rosacli.Client, profile *Profile) (ClusterHandler, error) {
	return newClusterHandler(client, profile, true, false)
}

// NewTempClusterHandler create a new cluster handler WITHOUT data persisted to Filesystem
// Useful for test cases needed resources. Do not forget to delete the resources afterwards
func NewTempClusterHandler(client *rosacli.Client, profile *Profile) (ClusterHandler, error) {
	return newClusterHandler(client, profile, false, false)
}

// NewClusterHandlerFromFilesystem create a new cluster handler from data saved on Filesystem
func NewClusterHandlerFromFilesystem(client *rosacli.Client, profile *Profile) (ClusterHandler, error) {
	return newClusterHandler(client, profile, true, true)
}

func newClusterHandler(client *rosacli.Client,
	profile *Profile,
	persist bool,
	loadFromFilesystem bool) (*clusterHandler, error) {

	var err error
	clusterDetail := &ClusterDetail{}
	clusterConfig := &ClusterConfigure.ClusterConfig{}
	if loadFromFilesystem {
		clusterDetail, err = ParseClusterDetail()
		if err != nil {
			return nil, err
		}
		clusterConfig, err = ClusterConfigure.ParseClusterProfile()
		if err != nil {
			return nil, err
		}

	}

	// Make sure shared VPC credentials file based on profile
	awsSharedCredentialsFile := ""
	if profile.ClusterConfig.SharedVPC || profile.ClusterConfig.AdditionalPrincipals {
		awsSharedCredentialsFile = config.Test.GlobalENV.SVPC_CREDENTIALS_FILE
		if awsSharedCredentialsFile == "" {
			log.Logger.Errorf(envVariableErrMsg, awsSharedCredentialsFile)
			return nil, fmt.Errorf(envVariableErrMsg, awsSharedCredentialsFile)
		}
	}

	resourcesHandler, err := newResourcesHandler(
		client,
		profile.Region,
		persist,
		loadFromFilesystem,
		awsSharedCredentialsFile,
	)
	if err != nil {
		return nil, err
	}

	// Make sure shared VPC credentials file based on loaded resources
	if resourcesHandler.resources.SharedVPCRole != "" ||
		resourcesHandler.resources.AdditionalPrincipals != "" ||
		resourcesHandler.resources.ResourceShareArn != "" {
		awsSharedCredentialsFile = config.Test.GlobalENV.SVPC_CREDENTIALS_FILE
		if awsSharedCredentialsFile == "" {
			log.Logger.Errorf(envVariableErrMsg, awsSharedCredentialsFile)
			return nil, fmt.Errorf(envVariableErrMsg, awsSharedCredentialsFile)
		}
	}
	resourcesHandler.awsSharedCredentialsFile = awsSharedCredentialsFile

	return &clusterHandler{
		rosaClient:       client,
		profile:          profile,
		clusterDetail:    clusterDetail,
		clusterConfig:    clusterConfig,
		resourcesHandler: resourcesHandler,
	}, nil

}

func (ch *clusterHandler) saveToFile() error {
	var errs []error

	// Resources
	err := ch.resourcesHandler.saveToFile()
	if err != nil {
		errs = append(errs, err)
	}

	// Cluster Config
	_, err = helper.CreateFileWithContent(config.Test.ClusterConfigFile, &ch.clusterConfig)
	if err != nil {
		errs = append(errs, err)
	}

	// Cluster Detail
	_, err = helper.CreateFileWithContent(config.Test.ClusterDetailFile, &ch.clusterDetail)
	if err != nil {
		errs = append(errs, err)
	}

	// Temporary recoding file to make it compatible to existing jobs
	helper.CreateFileWithContent(config.Test.APIURLFile, ch.clusterDetail.APIURL)
	helper.CreateFileWithContent(config.Test.ConsoleUrlFile, ch.clusterDetail.ConsoleURL)
	helper.CreateFileWithContent(config.Test.InfraIDFile, ch.clusterDetail.InfraID)
	helper.CreateFileWithContent(config.Test.ClusterIDFile, ch.clusterDetail.ClusterID)
	helper.CreateFileWithContent(config.Test.ClusterNameFile, ch.clusterDetail.ClusterName)
	helper.CreateFileWithContent(config.Test.ClusterTypeFile, ch.clusterDetail.ClusterType)
	// End of temporarsy

	return errors.Join(errs...)
}

func (ch *clusterHandler) GetResourcesHandler() ResourcesHandler {
	return ch.resourcesHandler
}

// GenerateClusterCreateFlags will generate cluster creation flags
func (ch *clusterHandler) GenerateClusterCreateFlags() ([]string, error) {
	resourcesHandler := ch.resourcesHandler
	if ch.profile.ClusterConfig.NameLength == 0 {
		ch.profile.ClusterConfig.NameLength = constants.DefaultNameLength //Set to a default value when it is not set
	}
	if ch.profile.NamePrefix == "" {
		panic("The profile name prefix is empty. Please set with env variable NAME_PREFIX")
	}
	clusterName := resourcesHandler.PreparePrefix(ch.profile.NamePrefix, ch.profile.ClusterConfig.NameLength)
	ch.profile.ClusterConfig.Name = clusterName

	sharedVPCRoleArn := ""
	sharedVPCRolePrefix := ""
	defer func() {
		err := ch.saveToFile()
		if err != nil {
			log.Logger.Errorf("Cannot record data: %s", err.Error())
			panic(fmt.Errorf("cannot record data: %s", err.Error()))
		}
	}()
	flags := []string{"-y"}
	ch.clusterConfig.Name = clusterName

	if ch.profile.Version != "" {
		// Force set the hcp parameter to false since hcp cannot filter the upgrade versions
		version, err := resourcesHandler.PrepareVersion(ch.profile.Version, ch.profile.ChannelGroup, false)

		if err != nil {
			return flags, err
		}
		if version == nil {
			err = fmt.Errorf("cannot find a version match the condition %s", ch.profile.Version)
			return flags, err
		}
		ch.profile.Version = version.Version
		flags = append(flags, "--version", version.Version)

		ch.clusterConfig.Version = &ClusterConfigure.Version{
			ChannelGroup: ch.profile.ChannelGroup,
			RawID:        version.Version,
		}
	}
	if ch.profile.ChannelGroup != "" {
		flags = append(flags, "--channel-group", ch.profile.ChannelGroup)
		if ch.clusterConfig.Version == nil {
			ch.clusterConfig.Version = &ClusterConfigure.Version{}
		}
		ch.clusterConfig.Version.ChannelGroup = ch.profile.ChannelGroup
	}
	if ch.profile.Region != "" {
		flags = append(flags, "--region", ch.profile.Region)
		ch.clusterConfig.Region = ch.profile.Region
	}
	if ch.profile.ClusterConfig.DomainPrefixEnabled {
		flags = append(flags,
			"--domain-prefix", helper.TrimNameByLength(clusterName, ocm.MaxClusterDomainPrefixLength),
		)
	}
	if ch.profile.ClusterConfig.STS {
		var accRoles *rosacli.AccountRolesUnit
		var oidcConfigID string
		accountRolePrefix := helper.TrimNameByLength(clusterName, constants.MaxRolePrefixLength)
		log.Logger.Infof(
			"Got sts set to true. Going to prepare Account roles with prefix %s",
			accountRolePrefix,
		)
		accRoles, err := resourcesHandler.PrepareAccountRoles(
			accountRolePrefix,
			ch.profile.ClusterConfig.HCP,
			ch.profile.Version,
			ch.profile.ChannelGroup,
			ch.profile.AccountRoleConfig.Path,
			ch.profile.AccountRoleConfig.PermissionBoundary,
		)
		if err != nil {
			log.Logger.Errorf("Got error happens when preparing account roles: %s", err.Error())
			return flags, err
		}
		flags = append(flags,
			"--role-arn", accRoles.InstallerRole,
			"--support-role-arn", accRoles.SupportRole,
			"--worker-iam-role", accRoles.WorkerRole,
		)

		ch.clusterConfig.Sts = true
		ch.clusterConfig.Aws = &ClusterConfigure.AWS{
			Sts: ClusterConfigure.Sts{
				RoleArn:        accRoles.InstallerRole,
				SupportRoleArn: accRoles.SupportRole,
				WorkerRoleArn:  accRoles.WorkerRole,
			},
		}
		if !ch.profile.ClusterConfig.HCP {
			flags = append(flags,
				"--controlplane-iam-role", accRoles.ControlPlaneRole,
			)
			ch.clusterConfig.Aws.Sts.ControlPlaneRoleArn = accRoles.ControlPlaneRole

		}

		if ch.profile.ClusterConfig.SharedVPC {
			sharedVPCRolePrefix = accountRolePrefix
			awsClient, err := resourcesHandler.GetAWSClient(true)
			if err != nil {
				return flags, err
			}
			sharedVPCAccountID := awsClient.AccountID
			sharedVPCRoleArn = fmt.Sprintf("arn:aws:iam::%s:role/%s-shared-vpc-role", sharedVPCAccountID, sharedVPCRolePrefix)
		}

		operatorRolePrefix := accountRolePrefix
		if ch.profile.ClusterConfig.OIDCConfig != "" {
			oidcConfigPrefix := helper.TrimNameByLength(clusterName, constants.MaxOIDCConfigPrefixLength)
			log.Logger.Infof("Got  oidc config setting, going to prepare the %s oidc with prefix %s",
				ch.profile.ClusterConfig.OIDCConfig, oidcConfigPrefix)
			oidcConfigID, err = resourcesHandler.PrepareOIDCConfig(ch.profile.ClusterConfig.OIDCConfig,
				accRoles.InstallerRole, oidcConfigPrefix)
			if err != nil {
				return flags, err
			}
			err = resourcesHandler.PrepareOIDCProvider(oidcConfigID)
			if err != nil {
				return flags, err
			}
			err = resourcesHandler.PrepareOperatorRolesByOIDCConfig(operatorRolePrefix,
				oidcConfigID, accRoles.InstallerRole, sharedVPCRoleArn, ch.profile.ClusterConfig.HCP, ch.profile.ChannelGroup)
			if err != nil {
				return flags, err
			}
			flags = append(flags, "--oidc-config-id", oidcConfigID)
			ch.clusterConfig.Aws.Sts.OidcConfigID = oidcConfigID
		}

		flags = append(flags, "--operator-roles-prefix", operatorRolePrefix)
		ch.clusterConfig.Aws.Sts.OperatorRolesPrefix = operatorRolePrefix

		if ch.profile.ClusterConfig.SharedVPC {
			log.Logger.Info(
				"Got shared vpc settings. Going to sleep 30s to wait for the operator roles prepared")
			time.Sleep(30 * time.Second)
			installRoleArn := accRoles.InstallerRole
			ingressOperatorRoleArn := fmt.Sprintf("%s/%s-%s", strings.Split(installRoleArn, "/")[0],
				sharedVPCRolePrefix, "openshift-ingress-operator-cloud-credentials")
			_, sharedVPCRoleArn, err := resourcesHandler.PrepareSharedVPCRole(sharedVPCRolePrefix, installRoleArn,
				ingressOperatorRoleArn)
			if err != nil {
				return flags, err
			}
			flags = append(flags, "--shared-vpc-role-arn", sharedVPCRoleArn)
		}

		if ch.profile.ClusterConfig.AuditLogForward {
			auditLogRoleName := accountRolePrefix
			auditRoleArn, err := resourcesHandler.PrepareAuditlogRoleArnByOIDCConfig(auditLogRoleName, oidcConfigID)
			ch.clusterConfig.AuditLogArn = auditRoleArn
			if err != nil {
				return flags, err
			}
			flags = append(flags,
				"--audit-log-arn", auditRoleArn)
		}

		if ch.profile.ClusterConfig.AdditionalPrincipals {
			installRoleArn := accRoles.InstallerRole
			additionalPrincipalRolePrefix := accountRolePrefix
			additionalPrincipalRoleName := fmt.Sprintf("%s-%s", additionalPrincipalRolePrefix, "additional-principal-role")
			additionalPrincipalRoleArn, err := resourcesHandler.
				PrepareAdditionalPrincipalsRole(additionalPrincipalRoleName, installRoleArn)
			if err != nil {
				return flags, err
			}
			flags = append(flags, "--additional-allowed-principals", additionalPrincipalRoleArn)
			ch.clusterConfig.AdditionalPrincipals = additionalPrincipalRoleArn
		}
	}

	// Put this part before the BYOVPC preparation so the subnets is prepared based on PrivateLink
	if ch.profile.ClusterConfig.Private {
		flags = append(flags, "--private")
		ch.clusterConfig.Private = ch.profile.ClusterConfig.Private
		if ch.profile.ClusterConfig.HCP {
			ch.profile.ClusterConfig.PrivateLink = true
		}
	}

	if ch.profile.ClusterConfig.AdminEnabled {
		// Comment below part due to OCM-7112
		log.Logger.Infof("Day1 admin is enabled. Going to generate the admin user and password and record in %s",
			config.Test.ClusterAdminFile)
		_, password := resourcesHandler.PrepareAdminUser() // Unuse cluster-admin right now
		userName := "cluster-admin"

		flags = append(flags,
			"--create-admin-user",
			"--cluster-admin-password", password,
			// "--cluster-admin-user", userName,
		)
		helper.CreateFileWithContent(config.Test.ClusterAdminFile, fmt.Sprintf("%s:%s", userName, password))
	}

	if ch.profile.ClusterConfig.Autoscale {
		minReplicas := "3"
		maxRelicas := "6"
		flags = append(flags,
			"--enable-autoscaling",
			"--min-replicas", minReplicas,
			"--max-replicas", maxRelicas,
		)
		ch.clusterConfig.Autoscaling = &ClusterConfigure.Autoscaling{
			Enabled: true,
		}
		ch.clusterConfig.Nodes = &ClusterConfigure.Nodes{
			MinReplicas: minReplicas,
			MaxReplicas: maxRelicas,
		}
	}
	if ch.profile.ClusterConfig.WorkerPoolReplicas != 0 {
		flags = append(flags, "--replicas", fmt.Sprintf("%v", ch.profile.ClusterConfig.WorkerPoolReplicas))
		ch.clusterConfig.Nodes = &ClusterConfigure.Nodes{
			Replicas: fmt.Sprintf("%v", ch.profile.ClusterConfig.WorkerPoolReplicas),
		}
	}

	if ch.profile.ClusterConfig.IngressCustomized {
		ch.clusterConfig.IngressConfig = &ClusterConfigure.IngressConfig{
			DefaultIngressRouteSelector:            "app1=test1,app2=test2",
			DefaultIngressExcludedNamespaces:       "test-ns1,test-ns2",
			DefaultIngressWildcardPolicy:           "WildcardsDisallowed",
			DefaultIngressNamespaceOwnershipPolicy: "Strict",
		}
		flags = append(flags,
			"--default-ingress-route-selector",
			ch.clusterConfig.IngressConfig.DefaultIngressRouteSelector,
			"--default-ingress-excluded-namespaces",
			ch.clusterConfig.IngressConfig.DefaultIngressExcludedNamespaces,
			"--default-ingress-wildcard-policy",
			ch.clusterConfig.IngressConfig.DefaultIngressWildcardPolicy,
			"--default-ingress-namespace-ownership-policy",
			ch.clusterConfig.IngressConfig.DefaultIngressNamespaceOwnershipPolicy,
		)
	}
	if ch.profile.ClusterConfig.AutoscalerEnabled {
		if !ch.profile.ClusterConfig.Autoscale {
			return nil, errors.New("Autoscaler is enabled without having enabled the autoscale field") // nolint
		}
		autoscaler := &ClusterConfigure.Autoscaler{
			AutoscalerBalanceSimilarNodeGroups:    true,
			AutoscalerSkipNodesWithLocalStorage:   true,
			AutoscalerLogVerbosity:                "4",
			AutoscalerMaxPodGracePeriod:           "0",
			AutoscalerPodPriorityThreshold:        "0",
			AutoscalerIgnoreDaemonsetsUtilization: true,
			AutoscalerMaxNodeProvisionTime:        "10m",
			AutoscalerBalancingIgnoredLabels:      "aaa",
			AutoscalerMaxNodesTotal:               "100",
			AutoscalerMinCores:                    "0",
			AutoscalerMaxCores:                    "1000",
			AutoscalerMinMemory:                   "0",
			AutoscalerMaxMemory:                   "4096",
			// AutoscalerGpuLimit:                      "1",
			AutoscalerScaleDownEnabled:              true,
			AutoscalerScaleDownUtilizationThreshold: "0.5",
			AutoscalerScaleDownDelayAfterAdd:        "10s",
			AutoscalerScaleDownDelayAfterDelete:     "10s",
			AutoscalerScaleDownDelayAfterFailure:    "10s",
			// AutoscalerScaleDownUnneededTime:         "3m",
		}
		flags = append(flags,
			"--autoscaler-balance-similar-node-groups",
			"--autoscaler-skip-nodes-with-local-storage",
			"--autoscaler-log-verbosity", autoscaler.AutoscalerLogVerbosity,
			"--autoscaler-max-pod-grace-period", autoscaler.AutoscalerMaxPodGracePeriod,
			"--autoscaler-pod-priority-threshold", autoscaler.AutoscalerPodPriorityThreshold,
			"--autoscaler-ignore-daemonsets-utilization",
			"--autoscaler-max-node-provision-time", autoscaler.AutoscalerMaxNodeProvisionTime,
			"--autoscaler-balancing-ignored-labels", autoscaler.AutoscalerBalancingIgnoredLabels,
			"--autoscaler-max-nodes-total", autoscaler.AutoscalerMaxNodesTotal,
			"--autoscaler-min-cores", autoscaler.AutoscalerMinCores,
			"--autoscaler-max-cores", autoscaler.AutoscalerMaxCores,
			"--autoscaler-min-memory", autoscaler.AutoscalerMinMemory,
			"--autoscaler-max-memory", autoscaler.AutoscalerMaxMemory,
			// "--autoscaler-gpu-limit", autoscaler.AutoscalerGpuLimit,
			"--autoscaler-scale-down-enabled",
			// "--autoscaler-scale-down-unneeded-time", autoscaler.AutoscalerScaleDownUnneededTime,
			"--autoscaler-scale-down-utilization-threshold", autoscaler.AutoscalerScaleDownUtilizationThreshold,
			"--autoscaler-scale-down-delay-after-add", autoscaler.AutoscalerScaleDownDelayAfterAdd,
			"--autoscaler-scale-down-delay-after-delete", autoscaler.AutoscalerScaleDownDelayAfterDelete,
			"--autoscaler-scale-down-delay-after-failure", autoscaler.AutoscalerScaleDownDelayAfterFailure,
		)

		ch.clusterConfig.Autoscaler = autoscaler
	}
	if ch.profile.ClusterConfig.NetworkingSet {
		networking := &ClusterConfigure.Networking{
			MachineCIDR: "10.0.0.0/16",
			PodCIDR:     "192.168.0.0/18",
			ServiceCIDR: "172.31.0.0/24",
			HostPrefix:  "25",
		}
		flags = append(flags,
			"--machine-cidr", networking.MachineCIDR, // Placeholder, it should be vpc CIDR
			"--service-cidr", networking.ServiceCIDR,
			"--pod-cidr", networking.PodCIDR,
			"--host-prefix", networking.HostPrefix,
		)
		ch.clusterConfig.Networking = networking
	}
	if ch.profile.ClusterConfig.BYOVPC {
		var vpc *vpc_client.VPC
		var err error
		vpcPrefix := helper.TrimNameByLength(clusterName, 20)
		log.Logger.Info("Got BYOVPC set to true. Going to prepare subnets")
		cidrValue := constants.DefaultVPCCIDRValue
		if ch.profile.ClusterConfig.NetworkingSet {
			cidrValue = ch.clusterConfig.Networking.MachineCIDR
		}

		vpc, err = resourcesHandler.PrepareVPC(vpcPrefix, cidrValue, false)
		if err != nil {
			return flags, err
		}

		zones := strings.Split(ch.profile.ClusterConfig.Zones, ",")
		zones = helper.RemoveFromStringSlice(zones, "")
		subnets, err := resourcesHandler.PrepareSubnets(zones, ch.profile.ClusterConfig.MultiAZ)
		if err != nil {
			return flags, err
		}
		subnetsFlagValue := strings.Join(append(subnets["private"], subnets["public"]...), ",")
		ch.clusterConfig.Subnets = &ClusterConfigure.Subnets{
			PrivateSubnetIds: strings.Join(subnets["private"], ","),
			PublicSubnetIds:  strings.Join(subnets["public"], ","),
		}
		if ch.profile.ClusterConfig.PrivateLink {
			log.Logger.Info("Got private link set to true. Only set private subnets to cluster flags")
			subnetsFlagValue = strings.Join(subnets["private"], ",")
			ch.clusterConfig.Subnets = &ClusterConfigure.Subnets{
				PrivateSubnetIds: strings.Join(subnets["private"], ","),
			}
		}
		flags = append(flags,
			"--subnet-ids", subnetsFlagValue)

		if ch.profile.ClusterConfig.AdditionalSGNumber != 0 {
			securityGroups, err := resourcesHandler.
				PrepareAdditionalSecurityGroups(ch.profile.ClusterConfig.AdditionalSGNumber, vpcPrefix)
			if err != nil {
				return flags, err
			}
			computeSGs := strings.Join(securityGroups, ",")
			infraSGs := strings.Join(securityGroups, ",")
			controlPlaneSGs := strings.Join(securityGroups, ",")
			if ch.profile.ClusterConfig.HCP {
				flags = append(flags,
					"--additional-compute-security-group-ids", computeSGs,
				)
				ch.clusterConfig.AdditionalSecurityGroups = &ClusterConfigure.AdditionalSecurityGroups{
					WorkerSecurityGroups: computeSGs,
				}
			} else {
				flags = append(flags,
					"--additional-infra-security-group-ids", infraSGs,
					"--additional-control-plane-security-group-ids", controlPlaneSGs,
					"--additional-compute-security-group-ids", computeSGs,
				)
				ch.clusterConfig.AdditionalSecurityGroups = &ClusterConfigure.AdditionalSecurityGroups{
					ControlPlaneSecurityGroups: controlPlaneSGs,
					InfraSecurityGroups:        infraSGs,
					WorkerSecurityGroups:       computeSGs,
				}
			}
		}
		if ch.profile.ClusterConfig.ProxyEnabled {
			proxyName := vpc.VPCName
			if proxyName == "" {
				proxyName = clusterName
			}
			proxy, err := resourcesHandler.
				PrepareProxy(ch.profile.Region, proxyName, config.Test.OutputDir, config.Test.ProxyCABundleFile)
			if err != nil {
				return flags, err
			}

			ch.clusterConfig.Proxy = &ClusterConfigure.Proxy{
				Enabled:         ch.profile.ClusterConfig.ProxyEnabled,
				Http:            proxy.HTTPProxy,
				Https:           proxy.HTTPsProxy,
				NoProxy:         proxy.NoProxy,
				TrustBundleFile: proxy.CABundleFilePath,
			}
			flags = append(flags,
				"--http-proxy", proxy.HTTPProxy,
				"--https-proxy", proxy.HTTPsProxy,
				"--no-proxy", proxy.NoProxy,
				"--additional-trust-bundle-file", proxy.CABundleFilePath,
			)

		}
		if ch.profile.ClusterConfig.SharedVPC {
			subnetArns, err := resourcesHandler.PrepareSubnetArns(subnetsFlagValue)
			if err != nil {
				return flags, err
			}

			resourceShareName := fmt.Sprintf("%s-%s", sharedVPCRolePrefix, "resource-share")
			_, err = resourcesHandler.PrepareResourceShare(resourceShareName, subnetArns)
			if err != nil {
				return flags, err
			}

			dnsDomain, err := resourcesHandler.PrepareDNSDomain()
			if err != nil {
				return flags, err
			}
			flags = append(flags, "--base-domain", dnsDomain)

			hostedZoneID, err := resourcesHandler.PrepareHostedZone(clusterName, dnsDomain, vpc.VpcID, true)
			if err != nil {
				return flags, err
			}
			flags = append(flags, "--private-hosted-zone-id", hostedZoneID)

			ch.clusterConfig.SharedVPC = ch.profile.ClusterConfig.SharedVPC
		}
	}
	if ch.profile.ClusterConfig.BillingAccount != "" {
		flags = append(flags, "--billing-account", ch.profile.ClusterConfig.BillingAccount)
		ch.clusterConfig.BillingAccount = ch.profile.ClusterConfig.BillingAccount
	}
	if ch.profile.ClusterConfig.DisableSCPChecks {
		flags = append(flags, "--disable-scp-checks")
		ch.clusterConfig.DisableScpChecks = true
	}
	if ch.profile.ClusterConfig.DisableUserWorKloadMonitoring {
		flags = append(flags, "--disable-workload-monitoring")
		ch.clusterConfig.DisableWorkloadMonitoring = true
	}
	if ch.profile.ClusterConfig.EtcdKMS {
		keyArn, err := resourcesHandler.PrepareKMSKey(false, "rosacli", ch.profile.ClusterConfig.HCP, true)
		if err != nil {
			return flags, err
		}
		flags = append(flags,
			"--etcd-encryption-kms-arn", keyArn,
		)
		if ch.clusterConfig.Encryption == nil {
			ch.clusterConfig.Encryption = &ClusterConfigure.Encryption{}
		}
		ch.clusterConfig.Encryption.EtcdEncryptionKmsArn = keyArn
	}

	if ch.profile.ClusterConfig.Ec2MetadataHttpTokens != "" {
		flags = append(flags, "--ec2-metadata-http-tokens", ch.profile.ClusterConfig.Ec2MetadataHttpTokens)
		ch.clusterConfig.Ec2MetadataHttpTokens = ch.profile.ClusterConfig.Ec2MetadataHttpTokens
	}
	if ch.profile.ClusterConfig.EtcdEncryption {
		flags = append(flags, "--etcd-encryption")
		ch.clusterConfig.EtcdEncryption = ch.profile.ClusterConfig.EtcdEncryption

	}
	if ch.profile.ClusterConfig.ExternalAuthConfig {
		flags = append(flags, "--external-auth-providers-enabled")
		ch.clusterConfig.ExternalAuthentication = ch.profile.ClusterConfig.ExternalAuthConfig
	}

	if ch.profile.ClusterConfig.FIPS {
		flags = append(flags, "--fips")
	}
	if ch.profile.ClusterConfig.HCP {
		flags = append(flags, "--hosted-cp")
	}
	ch.clusterConfig.Nodes = &ClusterConfigure.Nodes{}
	if ch.profile.ClusterConfig.InstanceType != "" {
		flags = append(flags, "--compute-machine-type", ch.profile.ClusterConfig.InstanceType)
		ch.clusterConfig.Nodes.ComputeInstanceType = ch.profile.ClusterConfig.InstanceType
	} else {
		ch.clusterConfig.Nodes.ComputeInstanceType = constants.DefaultInstanceType
	}
	if ch.profile.ClusterConfig.KMSKey {
		kmsKeyArn, err := resourcesHandler.PrepareKMSKey(false, "rosacli", ch.profile.ClusterConfig.HCP, false)
		if err != nil {
			return flags, err
		}
		flags = append(flags,
			"--kms-key-arn", kmsKeyArn,
			"--enable-customer-managed-key",
		)
		if ch.clusterConfig.Encryption == nil {
			ch.clusterConfig.Encryption = &ClusterConfigure.Encryption{}
		}
		ch.clusterConfig.EnableCustomerManagedKey = ch.profile.ClusterConfig.KMSKey
		ch.clusterConfig.Encryption.KmsKeyArn = kmsKeyArn
	}
	if ch.profile.ClusterConfig.LabelEnabled {
		dmpLabel := "test-label/openshift.io=,test-label=testvalue"
		flags = append(flags, "--worker-mp-labels", dmpLabel)
		ch.clusterConfig.DefaultMpLabels = dmpLabel
	}
	if ch.profile.ClusterConfig.MultiAZ {
		flags = append(flags, "--multi-az")
		ch.clusterConfig.MultiAZ = ch.profile.ClusterConfig.MultiAZ
	}

	if ch.profile.ClusterConfig.PrivateLink {
		flags = append(flags, "--private-link")
		ch.clusterConfig.PrivateLink = ch.profile.ClusterConfig.PrivateLink

	}
	if ch.profile.ClusterConfig.ProvisionShard != "" {
		flags = append(flags, "--properties", fmt.Sprintf("provision_shard_id:%s", ch.profile.ClusterConfig.ProvisionShard))
		ch.clusterConfig.Properties = &ClusterConfigure.Properties{
			ProvisionShardID: ch.profile.ClusterConfig.ProvisionShard,
		}
	}

	if ch.profile.ClusterConfig.TagEnabled {
		tags := "test-tag:tagvalue,qe-managed:true"
		flags = append(flags, "--tags", tags)
		ch.clusterConfig.Tags = tags
	}
	if ch.profile.ClusterConfig.VolumeSize != 0 {
		diskSize := fmt.Sprintf("%dGiB", ch.profile.ClusterConfig.VolumeSize)
		flags = append(flags, "--worker-disk-size", diskSize)
		ch.clusterConfig.WorkerDiskSize = diskSize
	}
	if ch.profile.ClusterConfig.Zones != "" && !ch.profile.ClusterConfig.BYOVPC {
		flags = append(flags, "--availability-zones", ch.profile.ClusterConfig.Zones)
		ch.clusterConfig.AvailabilityZones = ch.profile.ClusterConfig.Zones
	}
	if ch.profile.ClusterConfig.ExternalAuthConfig {
		flags = append(flags, "--external-auth-providers-enabled")
	}
	if ch.profile.ClusterConfig.NetworkType == "other" {
		flags = append(flags, "--no-cni")
		ch.clusterConfig.Networking.Type = ch.profile.ClusterConfig.NetworkType
	}

	return flags, nil
}

func (ch *clusterHandler) WaitForClusterReady(timeoutMin int) error {
	var err error
	clusterID := ch.clusterDetail.ClusterID
	if clusterID == "" {
		return errors.New("No Cluster ID defined to wait for")
	}
	defer func() {
		log.Logger.Info("Going to record the necessary information")
		ch.saveToFile()
	}()
	clusterService := ch.rosaClient.Cluster
	err = clusterService.WaitForClusterPassWaiting(clusterID, 1, 2)
	if err != nil {
		return err
	}
	endTime := time.Now().Add(time.Duration(timeoutMin) * time.Minute)
	sleepTime := 0
	for time.Now().Before(endTime) {
		description, err := clusterService.DescribeClusterAndReflect(clusterID)
		if err != nil {
			return err
		}
		ch.clusterDetail.APIURL = description.APIURL
		ch.clusterDetail.ConsoleURL = description.ConsoleURL
		ch.clusterDetail.InfraID = description.InfraID
		switch description.State {
		case constants.Ready:
			log.Logger.Infof("Cluster %s is ready now.", clusterID)
			return nil
		case constants.Uninstalling:
			return fmt.Errorf("cluster %s is %s now. Cannot wait for it ready",
				clusterID, constants.Uninstalling)
		default:
			if strings.Contains(description.State, constants.Error) {
				log.Logger.Errorf("Cluster is in %s status now. Recording the installation log", constants.Error)
				ch.recordClusterInstallationLog()
				return fmt.Errorf("cluster %s is in %s state with reason: %s",
					clusterID, constants.Error, description.State)
			}
			if strings.Contains(description.State, constants.Pending) ||
				strings.Contains(description.State, constants.Installing) ||
				strings.Contains(description.State, constants.Validating) {
				time.Sleep(2 * time.Minute)
				continue
			}
			if strings.Contains(description.State, constants.Waiting) {
				log.Logger.Infof("Cluster is in status of %v, wait for ready", constants.Waiting)
				if sleepTime >= 6 {
					return fmt.Errorf("cluster stuck to %s status for more than 6 mins. "+
						"Check the user data preparation for roles", description.State)
				}
				sleepTime += 2
				time.Sleep(2 * time.Minute)
				continue
			}
			return fmt.Errorf("unknown cluster state %s", description.State)
		}

	}

	return fmt.Errorf("timeout for cluster ready waiting after %d mins", timeoutMin)
}

func (ch *clusterHandler) reverifyClusterNetwork() error {
	log.Logger.Infof("verify network of cluster %s ", ch.clusterDetail.ClusterID)
	_, err := ch.rosaClient.NetworkVerifier.CreateNetworkVerifierWithCluster(ch.clusterDetail.ClusterID)
	return err
}

func (ch *clusterHandler) recordClusterInstallationLog() error {
	output, err := ch.rosaClient.Cluster.InstallLog(ch.clusterDetail.ClusterID)
	if err != nil {
		return err
	}
	_, err = helper.CreateFileWithContent(config.Test.ClusterInstallLogArtifactFile, output.String())
	return err
}

func (ch *clusterHandler) GetClusterDetail() *ClusterDetail {
	return ch.clusterDetail
}

func (ch *clusterHandler) createClusterByProfileWithoutWaiting() error {
	clusterService := ch.rosaClient.Cluster
	flags, err := ch.GenerateClusterCreateFlags()
	if err != nil {
		log.Logger.Errorf("Error happened when generate flags: %s", err.Error())
		return err
	}
	log.Logger.Infof("User data and flags preparation finished")
	_, err, createCMD := clusterService.Create(ch.profile.ClusterConfig.Name, flags...)
	if err != nil {
		return err
	}
	helper.CreateFileWithContent(config.Test.CreateCommandFile, createCMD)
	log.Logger.Info("Cluster created successfully")
	description, err := clusterService.DescribeClusterAndReflect(ch.profile.ClusterConfig.Name)
	if err != nil {
		return err
	}
	defer func() {
		log.Logger.Info("Going to record the necessary information")
		ch.saveToFile()
	}()
	ch.clusterDetail.ClusterID = description.ID
	ch.clusterDetail.ClusterName = description.Name
	ch.clusterDetail.ClusterType = "rosa"
	ch.clusterDetail.OIDCEndpointURL = description.OIDCEndpointURL
	ch.clusterDetail.OperatorRoleArns = description.OperatorIAMRoles

	// Need to do the post step when cluster has no oidcconfig enabled
	if ch.profile.ClusterConfig.OIDCConfig == "" && ch.profile.ClusterConfig.STS {
		err = ch.resourcesHandler.PrepareOIDCProviderByCluster(description.ID)
		if err != nil {
			return err
		}
		err = ch.resourcesHandler.PrepareOperatorRolesByCluster(description.ID)
		if err != nil {
			return err
		}
	}
	// Need to decorate the KMS key
	if ch.profile.ClusterConfig.KMSKey && ch.profile.ClusterConfig.STS {
		err = ch.elaborateKMSKeyForSTSCluster(false)
		if err != nil {
			return err
		}
	}
	if ch.profile.ClusterConfig.EtcdKMS && ch.profile.ClusterConfig.STS {
		err = ch.elaborateKMSKeyForSTSCluster(true)
		if err != nil {
			return err
		}
	}
	return err
}
func (ch *clusterHandler) CreateCluster(waitForClusterReady bool) (err error) {

	err = ch.createClusterByProfileWithoutWaiting()
	if err != nil {
		return err
	}
	clusterID := ch.clusterDetail.ClusterID
	if ch.profile.ClusterConfig.BYOVPC {
		log.Logger.Infof("Reverify the network for the cluster %s to make sure it can be parsed", clusterID)
		ch.reverifyClusterNetwork()
	}
	if waitForClusterReady {
		log.Logger.Infof("Waiting for the cluster %s to ready", clusterID)
		err = ch.WaitForClusterReady(config.Test.GlobalENV.ClusterWaitingTime)
		if err != nil {
			return err
		}
	}
	return err
}

func (ch *clusterHandler) destroyCluster() (errors []error) {
	if ch.clusterDetail.ClusterID != "" {
		clusterService := ch.rosaClient.Cluster
		clusterID := ch.clusterDetail.ClusterID
		output, errDeleteCluster := clusterService.DeleteCluster(clusterID, "-y")
		if errDeleteCluster != nil {
			if strings.Contains(output.String(), fmt.Sprintf("There is no cluster with identifier or name '%s'", clusterID)) {
				log.Logger.Infof("Cluster %s not exists.", clusterID)
			} else {
				log.Logger.Errorf("Error happened when delete cluster: %s", output.String())
				errors = append(errors, errDeleteCluster)
				return
			}
		} else {
			log.Logger.Infof("Waiting for the cluster %s to be uninstalled", clusterID)
			err := clusterService.WaitForClusterPassUninstalled(clusterID, 2, config.Test.GlobalENV.ClusterWaitingTime)
			if err != nil {
				log.Logger.Errorf("Error happened when waiting cluster uninstall: %s", err.Error())
				errors = append(errors, err)
				return
			} else {
				log.Logger.Infof("Delete cluster %s successfully.", clusterID)
			}

			// Remove OIDC provider
			if ch.profile.ClusterConfig.STS {
				_, err = ch.rosaClient.OCMResource.DeleteOIDCProvider("-c", clusterID, "-y", "--mode", "auto")
				if err != nil {
					log.Logger.Errorf("Error happened when delete oidc provider: %s", err.Error())
					errors = append(errors, err)
				}
				log.Logger.Infof("Delete oidc provider successfully")
			}
		}
	}
	return
}

func (ch *clusterHandler) Destroy() (errors []error) {
	// destroy cluster
	errDestroyCluster := ch.destroyCluster()
	if len(errDestroyCluster) > 0 {
		errors = append(errors, errDestroyCluster...)
		return errors
	}

	// destroy ch.resourcesHandler.Prepared user data
	errDestroyUserData := ch.resourcesHandler.DestroyResources()
	if len(errDestroyUserData) > 0 {
		errors = append(errors, errDestroyUserData...)
	}
	return errors
}

func (ch *clusterHandler) elaborateKMSKeyForSTSCluster(etcdKMS bool) error {
	clusterID := ch.clusterDetail.ClusterID
	jsonData, err := ch.rosaClient.Cluster.GetJSONClusterDescription(clusterID)
	if err != nil {
		return err
	}
	accountRoles := []string{
		jsonData.DigString("aws", "sts", "role_arn"),
	}
	operaorRoleMap := map[string]string{}
	keyArn := jsonData.DigString("aws", "kms_key_arn")
	if etcdKMS {
		keyArn = jsonData.DigString("aws", "etcd_encryption", "kms_key_arn")
	}
	operatorRoles := jsonData.DigObject("aws", "sts", "operator_iam_roles").([]interface{})
	for _, operatorRole := range operatorRoles {
		role := operatorRole.(map[string]interface{})
		operaorRoleMap[role["name"].(string)] = role["role_arn"].(string)
	}
	region := jsonData.DigString("region", "id")
	isHCP := jsonData.DigBool("hypershift", "enabled")
	err = kms_key.ConfigKMSKeyPolicyForSTS(keyArn, region, isHCP, accountRoles, operaorRoleMap)
	if err != nil {
		log.Logger.Errorf(
			"Elaborate the KMS key %s for cluster %s failed: %s",
			keyArn,
			clusterID,
			err.Error())
	} else {
		log.Logger.Infof(
			"Elaborate the KMS key %s for cluster %s successfully",
			keyArn,
			clusterID)
	}

	return err
}
