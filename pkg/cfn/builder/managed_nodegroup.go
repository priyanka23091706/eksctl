package builder

import (
	"context"
	"fmt"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/weaveworks/eksctl/pkg/awsapi"

	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/pkg/errors"
	gfnec2 "github.com/weaveworks/goformation/v4/cloudformation/ec2"
	gfneks "github.com/weaveworks/goformation/v4/cloudformation/eks"
	gfnt "github.com/weaveworks/goformation/v4/cloudformation/types"
	corev1 "k8s.io/api/core/v1"

	api "github.com/weaveworks/eksctl/pkg/apis/eksctl.io/v1alpha5"
	"github.com/weaveworks/eksctl/pkg/nodebootstrap"
	instanceutils "github.com/weaveworks/eksctl/pkg/utils/instance"
	"github.com/weaveworks/eksctl/pkg/vpc"
)

// ManagedNodeGroupResourceSet defines the CloudFormation resources required for a managed nodegroup
type ManagedNodeGroupResourceSet struct {
	clusterConfig         *api.ClusterConfig
	forceAddCNIPolicy     bool
	nodeGroup             *api.ManagedNodeGroup
	launchTemplateFetcher *LaunchTemplateFetcher
	ec2API                awsapi.EC2
	vpcImporter           vpc.Importer
	bootstrapper          nodebootstrap.Bootstrapper
	*resourceSet
}

const ManagedNodeGroupResourceName = "ManagedNodeGroup"

// NewManagedNodeGroup creates a new ManagedNodeGroupResourceSet
func NewManagedNodeGroup(ec2API awsapi.EC2, cluster *api.ClusterConfig, nodeGroup *api.ManagedNodeGroup, launchTemplateFetcher *LaunchTemplateFetcher, bootstrapper nodebootstrap.Bootstrapper, forceAddCNIPolicy bool, vpcImporter vpc.Importer) *ManagedNodeGroupResourceSet {
	return &ManagedNodeGroupResourceSet{
		clusterConfig:         cluster,
		forceAddCNIPolicy:     forceAddCNIPolicy,
		nodeGroup:             nodeGroup,
		launchTemplateFetcher: launchTemplateFetcher,
		ec2API:                ec2API,
		resourceSet:           newResourceSet(),
		vpcImporter:           vpcImporter,
		bootstrapper:          bootstrapper,
	}
}

// AddAllResources adds all required CloudFormation resources
func (m *ManagedNodeGroupResourceSet) AddAllResources(ctx context.Context) error {
	m.resourceSet.template.Description = fmt.Sprintf(
		"%s (SSH access: %v) %s",
		"EKS Managed Nodes",
		api.IsEnabled(m.nodeGroup.SSH.Allow),
		"[created by eksctl]")

	m.template.Mappings[servicePrincipalPartitionMapName] = servicePrincipalPartitionMappings

	var nodeRole *gfnt.Value
	if m.nodeGroup.IAM.InstanceRoleARN == "" {
		if err := createRole(m.resourceSet, m.clusterConfig.IAM, m.nodeGroup.IAM, true, m.forceAddCNIPolicy); err != nil {
			return err
		}
		nodeRole = gfnt.MakeFnGetAttString(cfnIAMInstanceRoleName, "Arn")
	} else {
		nodeRole = gfnt.NewString(NormalizeARN(m.nodeGroup.IAM.InstanceRoleARN))
	}

	subnets, err := AssignSubnets(ctx, m.nodeGroup.NodeGroupBase, m.vpcImporter, m.clusterConfig, m.ec2API)
	if err != nil {
		return err
	}

	scalingConfig := gfneks.Nodegroup_ScalingConfig{}
	if m.nodeGroup.MinSize != nil {
		scalingConfig.MinSize = gfnt.NewInteger(*m.nodeGroup.MinSize)
	}
	if m.nodeGroup.MaxSize != nil {
		scalingConfig.MaxSize = gfnt.NewInteger(*m.nodeGroup.MaxSize)
	}
	if m.nodeGroup.DesiredCapacity != nil {
		scalingConfig.DesiredSize = gfnt.NewInteger(*m.nodeGroup.DesiredCapacity)
	}

	for k, v := range m.clusterConfig.Metadata.Tags {
		if _, exists := m.nodeGroup.Tags[k]; !exists {
			m.nodeGroup.Tags[k] = v
		}
	}

	taints, err := mapTaints(m.nodeGroup.Taints)
	if err != nil {
		return err
	}

	managedResource := &gfneks.Nodegroup{
		ClusterName:   gfnt.NewString(m.clusterConfig.Metadata.Name),
		NodegroupName: gfnt.NewString(m.nodeGroup.Name),
		ScalingConfig: &scalingConfig,
		Subnets:       subnets,
		NodeRole:      nodeRole,
		Labels:        m.nodeGroup.Labels,
		Tags:          m.nodeGroup.Tags,
		Taints:        taints,
	}

	if m.nodeGroup.UpdateConfig != nil {
		updateConfig := &gfneks.Nodegroup_UpdateConfig{}
		if m.nodeGroup.UpdateConfig.MaxUnavailable != nil {
			updateConfig.MaxUnavailable = gfnt.NewInteger(*m.nodeGroup.UpdateConfig.MaxUnavailable)
		}
		if m.nodeGroup.UpdateConfig.MaxUnavailablePercentage != nil {
			updateConfig.MaxUnavailablePercentage = gfnt.NewInteger(*m.nodeGroup.UpdateConfig.MaxUnavailablePercentage)
		}
		managedResource.UpdateConfig = updateConfig
	}

	if m.nodeGroup.Spot {
		// TODO use constant from SDK
		managedResource.CapacityType = gfnt.NewString("SPOT")
	}

	if m.nodeGroup.ReleaseVersion != "" {
		managedResource.ReleaseVersion = gfnt.NewString(m.nodeGroup.ReleaseVersion)
	}

	instanceTypes := m.nodeGroup.InstanceTypeList()

	makeAMIType := func() *gfnt.Value {
		return gfnt.NewString(getAMIType(m.nodeGroup, selectManagedInstanceType(m.nodeGroup)))
	}

	var launchTemplate *gfneks.Nodegroup_LaunchTemplateSpecification

	if m.nodeGroup.LaunchTemplate != nil {
		launchTemplateData, err := m.launchTemplateFetcher.Fetch(ctx, m.nodeGroup.LaunchTemplate)
		if err != nil {
			return err
		}
		if err := validateLaunchTemplate(launchTemplateData, m.nodeGroup); err != nil {
			return err
		}

		launchTemplate = &gfneks.Nodegroup_LaunchTemplateSpecification{
			Id: gfnt.NewString(m.nodeGroup.LaunchTemplate.ID),
		}
		if version := m.nodeGroup.LaunchTemplate.Version; version != nil {
			launchTemplate.Version = gfnt.NewString(*version)
		}

		if launchTemplateData.ImageId == nil {
			// TODO: what?
			if launchTemplateData.InstanceType == "" {
				managedResource.AmiType = makeAMIType()
			} else {
				managedResource.AmiType = gfnt.NewString(getAMIType(m.nodeGroup, string(launchTemplateData.InstanceType)))
			}
		}

		if launchTemplateData.InstanceType == "" {
			managedResource.InstanceTypes = gfnt.NewStringSlice(instanceTypes...)
		}
	} else {
		launchTemplateData, err := m.makeLaunchTemplateData(ctx)
		if err != nil {
			return err
		}
		if launchTemplateData.ImageId == nil {
			managedResource.AmiType = makeAMIType()
		}
		managedResource.InstanceTypes = gfnt.NewStringSlice(instanceTypes...)

		ltRef := m.newResource("LaunchTemplate", &gfnec2.LaunchTemplate{
			LaunchTemplateName: gfnt.MakeFnSubString(fmt.Sprintf("${%s}", gfnt.StackName)),
			LaunchTemplateData: launchTemplateData,
		})
		launchTemplate = &gfneks.Nodegroup_LaunchTemplateSpecification{
			Id: ltRef,
		}
	}

	managedResource.LaunchTemplate = launchTemplate
	m.newResource(ManagedNodeGroupResourceName, managedResource)
	return nil
}

func mapTaints(taints []api.NodeGroupTaint) ([]gfneks.Nodegroup_Taint, error) {
	var ret []gfneks.Nodegroup_Taint

	mapEffect := func(effect corev1.TaintEffect) string {
		switch effect {
		case corev1.TaintEffectNoSchedule:
			return eks.TaintEffectNoSchedule
		case corev1.TaintEffectPreferNoSchedule:
			return eks.TaintEffectPreferNoSchedule
		case corev1.TaintEffectNoExecute:
			return eks.TaintEffectNoExecute
		default:
			return ""
		}
	}

	for _, t := range taints {
		effect := mapEffect(t.Effect)
		if effect == "" {
			return nil, errors.Errorf("unexpected taint effect: %v", t.Effect)
		}
		ret = append(ret, gfneks.Nodegroup_Taint{
			Key:    gfnt.NewString(t.Key),
			Value:  gfnt.NewString(t.Value),
			Effect: gfnt.NewString(effect),
		})
	}
	return ret, nil
}

func selectManagedInstanceType(ng *api.ManagedNodeGroup) string {
	if len(ng.InstanceTypes) > 0 {
		for _, instanceType := range ng.InstanceTypes {
			if instanceutils.IsGPUInstanceType(instanceType) {
				return instanceType
			}
		}
		return ng.InstanceTypes[0]
	}
	return ng.InstanceType
}

func validateLaunchTemplate(launchTemplateData *ec2types.ResponseLaunchTemplateData, ng *api.ManagedNodeGroup) error {
	const mngFieldName = "managedNodeGroup"

	// TODO: test
	if launchTemplateData.InstanceType == "" {
		if len(ng.InstanceTypes) == 0 {
			return errors.Errorf("instance type must be set in the launch template if %s.instanceTypes is not specified", mngFieldName)
		}
	} else if len(ng.InstanceTypes) > 0 {
		return errors.Errorf("instance type must not be set in the launch template if %s.instanceTypes is specified", mngFieldName)
	}

	// Custom AMI
	if launchTemplateData.ImageId != nil {
		if launchTemplateData.UserData == nil {
			return errors.New("node bootstrapping script (UserData) must be set when using a custom AMI")
		}
		notSupportedErr := func(fieldName string) error {
			return errors.Errorf("cannot set %s.%s when launchTemplate.ImageId is set", mngFieldName, fieldName)

		}
		if ng.AMI != "" {
			return notSupportedErr("ami")
		}
		if ng.ReleaseVersion != "" {
			return notSupportedErr("releaseVersion")
		}
	}

	if launchTemplateData.IamInstanceProfile != nil && launchTemplateData.IamInstanceProfile.Arn != nil {
		return errors.New("IAM instance profile must not be set in the launch template")
	}

	return nil
}

func getAMIType(ng *api.ManagedNodeGroup, instanceType string) string {
	amiTypeMapping := map[string]struct {
		X86x64 string
		GPU    string
		ARM    string
	}{
		api.NodeImageFamilyAmazonLinux2: {
			X86x64: eks.AMITypesAl2X8664,
			GPU:    eks.AMITypesAl2X8664Gpu,
			ARM:    eks.AMITypesAl2Arm64,
		},
		api.NodeImageFamilyBottlerocket: {
			X86x64: eks.AMITypesBottlerocketX8664,
			ARM:    eks.AMITypesBottlerocketArm64,
		},
	}

	amiType, ok := amiTypeMapping[ng.AMIFamily]
	if !ok {
		return eks.AMITypesCustom
	}

	switch {
	case instanceutils.IsGPUInstanceType(instanceType):
		return amiType.GPU
	case instanceutils.IsARMInstanceType(instanceType):
		return amiType.ARM
	default:
		return amiType.X86x64
	}
}

// RenderJSON implements the ResourceSet interface
func (m *ManagedNodeGroupResourceSet) RenderJSON() ([]byte, error) {
	return m.resourceSet.renderJSON()
}

// WithIAM implements the ResourceSet interface
func (m *ManagedNodeGroupResourceSet) WithIAM() bool {
	// eksctl does not support passing pre-created IAM instance roles to Managed Nodes,
	// so the IAM capability is always required
	return true
}

// WithNamedIAM implements the ResourceSet interface
func (m *ManagedNodeGroupResourceSet) WithNamedIAM() bool {
	return m.nodeGroup.IAM.InstanceRoleName != ""
}
