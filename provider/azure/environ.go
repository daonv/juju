// Copyright 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package azure

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/juju/errors"
	"github.com/juju/utils"
	"github.com/juju/utils/set"
	"launchpad.net/gwacl"

	"github.com/juju/juju/apiserver/params"
	"github.com/juju/juju/constraints"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/environs/config"
	"github.com/juju/juju/environs/imagemetadata"
	"github.com/juju/juju/environs/instances"
	"github.com/juju/juju/environs/simplestreams"
	"github.com/juju/juju/environs/storage"
	"github.com/juju/juju/instance"
	"github.com/juju/juju/network"
	"github.com/juju/juju/provider/common"
	"github.com/juju/juju/state"
)

const (
	// deploymentSlot says in which slot to deploy instances.  Azure
	// supports 'Production' or 'Staging'.
	// This provider always deploys to Production.  Think twice about
	// changing that: DNS names in the staging slot work differently from
	// those in the production slot.  In Staging, Azure assigns an
	// arbitrary hostname that we can then extract from the deployment's
	// URL.  In Production, the hostname in the deployment URL does not
	// actually seem to resolve; instead, the service name is used as the
	// DNS name, with ".cloudapp.net" appended.
	deploymentSlot = "Production"

	// Address space of the virtual network used by the nodes in this
	// environement, in CIDR notation. This is the network used for
	// machine-to-machine communication.
	networkDefinition = "10.0.0.0/8"

	// stateServerLabel is the label applied to the cloud service created
	// for state servers.
	stateServerLabel = "juju-state-server"
)

// vars for testing purposes.
var (
	createInstance = (*azureEnviron).createInstance
)

type azureEnviron struct {
	// Except where indicated otherwise, all fields in this object should
	// only be accessed using a lock or a snapshot.
	sync.Mutex

	// archMutex gates access to supportedArchitectures
	archMutex sync.Mutex
	// supportedArchitectures caches the architectures
	// for which images can be instantiated.
	supportedArchitectures []string

	// ecfg is the environment's Azure-specific configuration.
	ecfg *azureEnvironConfig

	// storage is this environ's own private storage.
	storage storage.Storage

	// storageAccountKey holds an access key to this environment's
	// private storage.  This is automatically queried from Azure on
	// startup.
	storageAccountKey string
}

// azureEnviron implements Environ and HasRegion.
var _ environs.Environ = (*azureEnviron)(nil)
var _ simplestreams.HasRegion = (*azureEnviron)(nil)
var _ state.Prechecker = (*azureEnviron)(nil)

// NewEnviron creates a new azureEnviron.
func NewEnviron(cfg *config.Config) (*azureEnviron, error) {
	var env azureEnviron
	err := env.SetConfig(cfg)
	if err != nil {
		return nil, err
	}

	// Set up storage.
	env.storage = &azureStorage{
		storageContext: &environStorageContext{environ: &env},
	}
	return &env, nil
}

// extractStorageKey returns the primary account key from a gwacl
// StorageAccountKeys struct, or if there is none, the secondary one.
func extractStorageKey(keys *gwacl.StorageAccountKeys) string {
	if keys.Primary != "" {
		return keys.Primary
	}
	return keys.Secondary
}

// queryStorageAccountKey retrieves the storage account's key from Azure.
func (env *azureEnviron) queryStorageAccountKey() (string, error) {
	azure, err := env.getManagementAPI()
	if err != nil {
		return "", err
	}
	defer env.releaseManagementAPI(azure)

	accountName := env.getSnapshot().ecfg.storageAccountName()
	keys, err := azure.GetStorageAccountKeys(accountName)
	if err != nil {
		return "", fmt.Errorf("cannot obtain storage account keys: %v", err)
	}

	key := extractStorageKey(keys)
	if key == "" {
		return "", fmt.Errorf("no keys available for storage account")
	}

	return key, nil
}

// getSnapshot produces an atomic shallow copy of the environment object.
// Whenever you need to access the environment object's fields without
// modifying them, get a snapshot and read its fields instead.  You will
// get a consistent view of the fields without any further locking.
// If you do need to modify the environment's fields, do not get a snapshot
// but lock the object throughout the critical section.
func (env *azureEnviron) getSnapshot() *azureEnviron {
	env.Lock()
	defer env.Unlock()

	// Copy the environment.  (Not the pointer, the environment itself.)
	// This is a shallow copy.
	snap := *env
	// Reset the snapshot's mutex, because we just copied it while we
	// were holding it.  The snapshot will have a "clean," unlocked mutex.
	snap.Mutex = sync.Mutex{}
	return &snap
}

// getAffinityGroupName returns the name of the affinity group used by all
// the Services in this environment.
func (env *azureEnviron) getAffinityGroupName() string {
	return env.getEnvPrefix() + "ag"
}

func (env *azureEnviron) createAffinityGroup() error {
	affinityGroupName := env.getAffinityGroupName()
	azure, err := env.getManagementAPI()
	if err != nil {
		return err
	}
	defer env.releaseManagementAPI(azure)
	snap := env.getSnapshot()
	location := snap.ecfg.location()
	cag := gwacl.NewCreateAffinityGroup(affinityGroupName, affinityGroupName, affinityGroupName, location)
	return azure.CreateAffinityGroup(&gwacl.CreateAffinityGroupRequest{
		CreateAffinityGroup: cag})
}

func (env *azureEnviron) deleteAffinityGroup() error {
	affinityGroupName := env.getAffinityGroupName()
	azure, err := env.getManagementAPI()
	if err != nil {
		return err
	}
	defer env.releaseManagementAPI(azure)
	return azure.DeleteAffinityGroup(&gwacl.DeleteAffinityGroupRequest{
		Name: affinityGroupName})
}

// getVirtualNetworkName returns the name of the virtual network used by all
// the VMs in this environment.
func (env *azureEnviron) getVirtualNetworkName() string {
	return env.getEnvPrefix() + "vnet"
}

func (env *azureEnviron) createVirtualNetwork() error {
	// Note: the Azure documentation recommends to use
	// Location when creating virtual network sites.
	// We have historically used Affinity Group, and
	// have observed intermittent issues when switching.
	// http://msdn.microsoft.com/en-us/library/azure/jj157100.aspx
	vnetName := env.getVirtualNetworkName()
	azure, err := env.getManagementAPI()
	if err != nil {
		return err
	}
	defer env.releaseManagementAPI(azure)
	virtualNetwork := gwacl.VirtualNetworkSite{
		Name:          vnetName,
		AffinityGroup: env.getAffinityGroupName(),
		AddressSpacePrefixes: []string{
			networkDefinition,
		},
	}
	return azure.AddVirtualNetworkSite(&virtualNetwork)
}

// deleteVnetAttempt is an AttemptyStrategy for use
// when attempting delete a virtual network. This is
// necessary as Azure apparently does not release all
// references to the vnet even when all cloud services
// are deleted.
var deleteVnetAttempt = utils.AttemptStrategy{
	Total: 30 * time.Second,
	Delay: 1 * time.Second,
}

var networkInUse = regexp.MustCompile(".*The virtual network .* is currently in use.*")

func (env *azureEnviron) deleteVirtualNetwork() error {
	azure, err := env.getManagementAPI()
	if err != nil {
		return err
	}
	defer env.releaseManagementAPI(azure)
	for a := deleteVnetAttempt.Start(); a.Next(); {
		vnetName := env.getVirtualNetworkName()
		err = azure.RemoveVirtualNetworkSite(vnetName)
		if err == nil {
			return nil
		}
		if err, ok := err.(*gwacl.AzureError); ok {
			if err.StatusCode() == 400 && networkInUse.MatchString(err.Message) {
				// Retry on "virtual network XYZ is currently in use".
				continue
			}
		}
		// Any other error should be returned.
		break
	}
	return err
}

// getContainerName returns the name of the private storage account container
// that this environment is using.
func (env *azureEnviron) getContainerName() string {
	return env.getEnvPrefix() + "private"
}

func isHTTPConflict(err error) bool {
	if err, ok := err.(gwacl.HTTPError); ok {
		return err.StatusCode() == http.StatusConflict
	}
	return false
}

func isVirtualNetworkExist(err error) bool {
	// TODO(axw) 2014-06-16 #1330473
	// Add an error type to gwacl for this.
	s := err.Error()
	const prefix = "could not add virtual network"
	const suffix = "already exists"
	return strings.HasPrefix(s, prefix) && strings.HasSuffix(s, suffix)
}

// Bootstrap is specified in the Environ interface.
func (env *azureEnviron) Bootstrap(ctx environs.BootstrapContext, args environs.BootstrapParams) (arch, series string, _ environs.BootstrapFinalizer, err error) {
	// The creation of the affinity group and the virtual network is specific to the Azure provider.
	err = env.createAffinityGroup()
	if err != nil && !isHTTPConflict(err) {
		return "", "", nil, err
	}
	// If we fail after this point, clean up the affinity group.
	defer func() {
		if err != nil {
			env.deleteAffinityGroup()
		}
	}()

	err = env.createVirtualNetwork()
	if err != nil && !isVirtualNetworkExist(err) {
		return "", "", nil, err
	}
	// If we fail after this point, clean up the virtual network.
	defer func() {
		if err != nil {
			env.deleteVirtualNetwork()
		}
	}()
	return common.Bootstrap(ctx, env, args)
}

// isLegacyInstance reports whether the instance is a
// legacy instance (i.e. one-to-one cloud service to instance).
func isLegacyInstance(inst *azureInstance) (bool, error) {
	context, err := inst.environ.getManagementAPI()
	if err != nil {
		return false, err
	}
	defer inst.environ.releaseManagementAPI(context)
	serviceName := inst.hostedService.ServiceName
	service, err := context.GetHostedServiceProperties(serviceName, true)
	if err != nil {
		return false, err
	} else if len(service.Deployments) != 1 {
		return false, nil
	}
	deploymentName := service.Deployments[0].Name
	return deploymentName == deploymentNameV1(serviceName), nil
}

// StateServerInstances is specified in the Environ interface.
func (env *azureEnviron) StateServerInstances() ([]instance.Id, error) {
	// Locate the state-server cloud service, and get its addresses.
	instances, err := env.AllInstances()
	if err != nil {
		return nil, err
	}
	var stateServerInstanceIds []instance.Id
	var loadStateFile bool
	for _, inst := range instances {
		azureInstance := inst.(*azureInstance)
		label := azureInstance.hostedService.Label
		if decoded, err := base64.StdEncoding.DecodeString(label); err == nil {
			if string(decoded) == stateServerLabel {
				stateServerInstanceIds = append(stateServerInstanceIds, inst.Id())
				continue
			}
		}
		if !loadStateFile {
			_, roleName := env.splitInstanceId(azureInstance.Id())
			if roleName == "" {
				loadStateFile = true
			}
		}
	}
	if loadStateFile {
		// Some legacy instances were found, so we must load provider-state
		// to find which instance was the original state server. If we find
		// a legacy environment, then stateServerInstanceIds will not contain
		// the original bootstrap instance, which is the only one that will
		// be in provider-state.
		instanceIds, err := common.ProviderStateInstances(env, env.Storage())
		if err != nil {
			return nil, err
		}
		stateServerInstanceIds = append(stateServerInstanceIds, instanceIds...)
	}
	if len(stateServerInstanceIds) == 0 {
		return nil, environs.ErrNotBootstrapped
	}
	return stateServerInstanceIds, nil
}

// Config is specified in the Environ interface.
func (env *azureEnviron) Config() *config.Config {
	snap := env.getSnapshot()
	return snap.ecfg.Config
}

// SetConfig is specified in the Environ interface.
func (env *azureEnviron) SetConfig(cfg *config.Config) error {
	ecfg, err := azureEnvironProvider{}.newConfig(cfg)
	if err != nil {
		return err
	}

	env.Lock()
	defer env.Unlock()

	if env.ecfg != nil {
		_, err = azureEnvironProvider{}.Validate(cfg, env.ecfg.Config)
		if err != nil {
			return err
		}
	}

	env.ecfg = ecfg

	// Reset storage account key.  Even if we had one before, it may not
	// be appropriate for the new config.
	env.storageAccountKey = ""

	return nil
}

// attemptCreateService tries to create a new hosted service on Azure, with a
// name it chooses (based on the given prefix), but recognizes that the name
// may not be available.  If the name is not available, it does not treat that
// as an error but just returns nil.
func attemptCreateService(azure *gwacl.ManagementAPI, prefix, affinityGroupName, label string) (*gwacl.CreateHostedService, error) {
	var err error
	name := gwacl.MakeRandomHostedServiceName(prefix)
	err = azure.CheckHostedServiceNameAvailability(name)
	if err != nil {
		// The calling function should retry.
		return nil, nil
	}
	if label == "" {
		label = name
	}
	req := gwacl.NewCreateHostedServiceWithLocation(name, label, "")
	req.AffinityGroup = affinityGroupName
	err = azure.AddHostedService(req)
	if err != nil {
		return nil, err
	}
	return req, nil
}

// newHostedService creates a hosted service.  It will make up a unique name,
// starting with the given prefix.
func newHostedService(azure *gwacl.ManagementAPI, prefix, affinityGroupName, label string) (*gwacl.HostedService, error) {
	var err error
	var createdService *gwacl.CreateHostedService
	for tries := 10; tries > 0 && err == nil && createdService == nil; tries-- {
		createdService, err = attemptCreateService(azure, prefix, affinityGroupName, label)
	}
	if err != nil {
		return nil, fmt.Errorf("could not create hosted service: %v", err)
	}
	if createdService == nil {
		return nil, fmt.Errorf("could not come up with a unique hosted service name - is your randomizer initialized?")
	}
	return azure.GetHostedServiceProperties(createdService.ServiceName, true)
}

// SupportedArchitectures is specified on the EnvironCapability interface.
func (env *azureEnviron) SupportedArchitectures() ([]string, error) {
	env.archMutex.Lock()
	defer env.archMutex.Unlock()
	if env.supportedArchitectures != nil {
		return env.supportedArchitectures, nil
	}
	// Create a filter to get all images from our region and for the correct stream.
	ecfg := env.getSnapshot().ecfg
	region := ecfg.location()
	cloudSpec := simplestreams.CloudSpec{
		Region:   region,
		Endpoint: getEndpoint(region),
	}
	imageConstraint := imagemetadata.NewImageConstraint(simplestreams.LookupParams{
		CloudSpec: cloudSpec,
		Stream:    ecfg.ImageStream(),
	})
	var err error
	env.supportedArchitectures, err = common.SupportedArchitectures(env, imageConstraint)
	return env.supportedArchitectures, err
}

// SupportNetworks is specified on the EnvironCapability interface.
func (env *azureEnviron) SupportNetworks() bool {
	return false
}

// SupportAddressAllocation is specified on the EnvironCapability interface.
func (e *azureEnviron) SupportAddressAllocation(netId network.Id) (bool, error) {
	return false, nil
}

// selectInstanceTypeAndImage returns the appropriate instances.InstanceType and
// the OS image name for launching a virtual machine with the given parameters.
func (env *azureEnviron) selectInstanceTypeAndImage(constraint *instances.InstanceConstraint) (*instances.InstanceType, string, error) {
	ecfg := env.getSnapshot().ecfg
	sourceImageName := ecfg.forceImageName()
	if sourceImageName != "" {
		// Configuration forces us to use a specific image.  There may
		// not be a suitable image in the simplestreams database.
		// This means we can't use Juju's normal selection mechanism,
		// because it combines instance-type and image selection: if
		// there are no images we can use, it won't offer us an
		// instance type either.
		//
		// Select the instance type using simple, Azure-specific code.
		instanceType, err := selectMachineType(env, defaultToBaselineSpec(constraint.Constraints))
		if err != nil {
			return nil, "", err
		}
		return instanceType, sourceImageName, nil
	}

	// Choose the most suitable instance type and OS image, based on simplestreams information.
	spec, err := findInstanceSpec(env, constraint)
	if err != nil {
		return nil, "", err
	}
	return &spec.InstanceType, spec.Image.Id, nil
}

var unsupportedConstraints = []string{
	constraints.CpuPower,
	constraints.Tags,
}

// ConstraintsValidator is defined on the Environs interface.
func (env *azureEnviron) ConstraintsValidator() (constraints.Validator, error) {
	validator := constraints.NewValidator()
	validator.RegisterUnsupported(unsupportedConstraints)
	supportedArches, err := env.SupportedArchitectures()
	if err != nil {
		return nil, err
	}
	validator.RegisterVocabulary(constraints.Arch, supportedArches)

	instanceTypes, err := listInstanceTypes(env)
	if err != nil {
		return nil, err
	}
	instTypeNames := make([]string, len(instanceTypes))
	for i, instanceType := range instanceTypes {
		instTypeNames[i] = instanceType.Name
	}
	validator.RegisterVocabulary(constraints.InstanceType, instTypeNames)
	validator.RegisterConflicts(
		[]string{constraints.InstanceType},
		[]string{constraints.Mem, constraints.CpuCores, constraints.Arch, constraints.RootDisk})

	return validator, nil
}

// PrecheckInstance is defined on the state.Prechecker interface.
func (env *azureEnviron) PrecheckInstance(series string, cons constraints.Value, placement string) error {
	if placement != "" {
		return fmt.Errorf("unknown placement directive: %s", placement)
	}
	if !cons.HasInstanceType() {
		return nil
	}
	// Constraint has an instance-type constraint so let's see if it is valid.
	instanceTypes, err := listInstanceTypes(env)
	if err != nil {
		return err
	}
	for _, instanceType := range instanceTypes {
		if instanceType.Name == *cons.InstanceType {
			return nil
		}
	}
	return fmt.Errorf("invalid instance type %q", *cons.InstanceType)
}

// createInstance creates all of the Azure entities necessary for a
// new instance. This includes Cloud Service, Deployment and Role.
//
// If serviceName is non-empty, then createInstance will assign to
// the Cloud Service with that name. Otherwise, a new Cloud Service
// will be created.
func (env *azureEnviron) createInstance(azure *gwacl.ManagementAPI, role *gwacl.Role, serviceName string, stateServer bool) (resultInst instance.Instance, resultErr error) {
	var inst instance.Instance
	defer func() {
		if inst != nil && resultErr != nil {
			if err := env.StopInstances(inst.Id()); err != nil {
				// Failure upon failure. Log it, but return the original error.
				logger.Errorf("error releasing failed instance: %v", err)
			}
		}
	}()
	var err error
	var service *gwacl.HostedService
	if serviceName != "" {
		logger.Debugf("creating instance in existing cloud service %q", serviceName)
		service, err = azure.GetHostedServiceProperties(serviceName, true)
	} else {
		logger.Debugf("creating instance in new cloud service")
		// If we're creating a cloud service for state servers,
		// we will want to open additional ports. We need to
		// record this against the cloud service, so we use a
		// special label for the purpose.
		var label string
		if stateServer {
			label = stateServerLabel
		}
		service, err = newHostedService(azure, env.getEnvPrefix(), env.getAffinityGroupName(), label)
	}
	if err != nil {
		return nil, err
	}
	if len(service.Deployments) == 0 {
		// This is a newly created cloud service, so we
		// should destroy it if anything below fails.
		defer func() {
			if resultErr != nil {
				azure.DeleteHostedService(service.ServiceName)
				// Destroying the hosted service destroys the instance,
				// so ensure StopInstances isn't called.
				inst = nil
			}
		}()
		// Create an initial deployment.
		deployment := gwacl.NewDeploymentForCreateVMDeployment(
			deploymentNameV2(service.ServiceName),
			deploymentSlot,
			deploymentNameV2(service.ServiceName),
			[]gwacl.Role{*role},
			env.getVirtualNetworkName(),
		)
		if err := azure.AddDeployment(deployment, service.ServiceName); err != nil {
			return nil, err
		}
		service.Deployments = append(service.Deployments, *deployment)
	} else {
		// Update the deployment.
		deployment := &service.Deployments[0]
		if err := azure.AddRole(&gwacl.AddRoleRequest{
			ServiceName:      service.ServiceName,
			DeploymentName:   deployment.Name,
			PersistentVMRole: (*gwacl.PersistentVMRole)(role),
		}); err != nil {
			return nil, err
		}
		deployment.RoleList = append(deployment.RoleList, *role)
	}
	return env.getInstance(service, role.RoleName)
}

// deploymentNameV1 returns the deployment name used
// in the original implementation of the Azure provider.
func deploymentNameV1(serviceName string) string {
	return serviceName
}

// deploymentNameV2 returns the deployment name used
// in the current implementation of the Azure provider.
func deploymentNameV2(serviceName string) string {
	return serviceName + "-v2"
}

// StartInstance is specified in the InstanceBroker interface.
func (env *azureEnviron) StartInstance(args environs.StartInstanceParams) (_ instance.Instance, _ *instance.HardwareCharacteristics, _ []network.Info, err error) {
	if args.MachineConfig.HasNetworks() {
		return nil, nil, nil, fmt.Errorf("starting instances with networks is not supported yet.")
	}

	err = environs.FinishMachineConfig(args.MachineConfig, env.Config())
	if err != nil {
		return nil, nil, nil, err
	}

	// Pick envtools.  Needed for the custom data (which is what we normally
	// call userdata).
	args.MachineConfig.Tools = args.Tools[0]
	logger.Infof("picked tools %q", args.MachineConfig.Tools)

	// Compose userdata.
	userData, err := makeCustomData(args.MachineConfig)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("custom data: %v", err)
	}

	azure, err := env.getManagementAPI()
	if err != nil {
		return nil, nil, nil, err
	}
	defer env.releaseManagementAPI(azure)

	snapshot := env.getSnapshot()
	location := snapshot.ecfg.location()
	instanceType, sourceImageName, err := env.selectInstanceTypeAndImage(&instances.InstanceConstraint{
		Region:      location,
		Series:      args.Tools.OneSeries(),
		Arches:      args.Tools.Arches(),
		Constraints: args.Constraints,
	})
	if err != nil {
		return nil, nil, nil, err
	}

	// We use the cloud service label as a way to group instances with
	// the same affinity, so that machines can be be allocated to the
	// same availability set.
	var cloudServiceName string
	if args.DistributionGroup != nil && snapshot.ecfg.availabilitySetsEnabled() {
		instanceIds, err := args.DistributionGroup()
		if err != nil {
			return nil, nil, nil, err
		}
		for _, id := range instanceIds {
			cloudServiceName, _ = env.splitInstanceId(id)
			if cloudServiceName != "" {
				break
			}
		}
	}

	vhd := env.newOSDisk(sourceImageName)
	// If we're creating machine-0, we'll want to expose port 22.
	// All other machines get an auto-generated public port for SSH.
	stateServer := false
	for _, job := range args.MachineConfig.Jobs {
		if job == params.JobManageEnviron {
			stateServer = true
			break
		}
	}
	role := env.newRole(instanceType.Id, vhd, userData, stateServer)
	inst, err := createInstance(env, azure.ManagementAPI, role, cloudServiceName, stateServer)
	if err != nil {
		return nil, nil, nil, err
	}
	hc := &instance.HardwareCharacteristics{
		Mem:      &instanceType.Mem,
		RootDisk: &instanceType.RootDisk,
		CpuCores: &instanceType.CpuCores,
	}
	if len(instanceType.Arches) == 1 {
		hc.Arch = &instanceType.Arches[0]
	}
	return inst, hc, nil, nil
}

// getInstance returns an up-to-date version of the instance with the given
// name.
func (env *azureEnviron) getInstance(hostedService *gwacl.HostedService, roleName string) (instance.Instance, error) {
	if n := len(hostedService.Deployments); n != 1 {
		return nil, fmt.Errorf("expected one deployment for %q, got %d", hostedService.ServiceName, n)
	}
	deployment := &hostedService.Deployments[0]

	var maskStateServerPorts bool
	var instanceId instance.Id
	switch deployment.Name {
	case deploymentNameV1(hostedService.ServiceName):
		// Old style instance.
		instanceId = instance.Id(hostedService.ServiceName)
		if n := len(deployment.RoleList); n != 1 {
			return nil, fmt.Errorf("expected one role for %q, got %d", deployment.Name, n)
		}
		roleName = deployment.RoleList[0].RoleName
		// In the old implementation of the Azure provider,
		// all machines opened the state and API server ports.
		maskStateServerPorts = true

	case deploymentNameV2(hostedService.ServiceName):
		instanceId = instance.Id(fmt.Sprintf("%s-%s", hostedService.ServiceName, roleName))
		// Newly created state server machines are put into
		// the cloud service with the stateServerLabel label.
		if decoded, err := base64.StdEncoding.DecodeString(hostedService.Label); err == nil {
			maskStateServerPorts = string(decoded) == stateServerLabel
		}
	}

	var roleInstance *gwacl.RoleInstance
	for _, role := range deployment.RoleInstanceList {
		if role.RoleName == roleName {
			roleInstance = &role
			break
		}
	}

	instance := &azureInstance{
		environ:              env,
		hostedService:        &hostedService.HostedServiceDescriptor,
		instanceId:           instanceId,
		deploymentName:       deployment.Name,
		roleName:             roleName,
		roleInstance:         roleInstance,
		maskStateServerPorts: maskStateServerPorts,
	}
	return instance, nil
}

// newOSDisk creates a gwacl.OSVirtualHardDisk object suitable for an
// Azure Virtual Machine.
func (env *azureEnviron) newOSDisk(sourceImageName string) *gwacl.OSVirtualHardDisk {
	vhdName := gwacl.MakeRandomDiskName("juju")
	vhdPath := fmt.Sprintf("vhds/%s", vhdName)
	snap := env.getSnapshot()
	storageAccount := snap.ecfg.storageAccountName()
	mediaLink := gwacl.CreateVirtualHardDiskMediaLink(storageAccount, vhdPath)
	// The disk label is optional and the disk name can be omitted if
	// mediaLink is provided.
	return gwacl.NewOSVirtualHardDisk("", "", "", mediaLink, sourceImageName, "Linux")
}

// getInitialEndpoints returns a slice of the endpoints every instance should have open
// (ssh port, etc).
func (env *azureEnviron) getInitialEndpoints(stateServer bool) []gwacl.InputEndpoint {
	cfg := env.Config()
	endpoints := []gwacl.InputEndpoint{{
		LocalPort: 22,
		Name:      "sshport",
		Port:      22,
		Protocol:  "tcp",
	}}
	if stateServer {
		endpoints = append(endpoints, []gwacl.InputEndpoint{{
			LocalPort: cfg.APIPort(),
			Port:      cfg.APIPort(),
			Protocol:  "tcp",
			Name:      "apiport",
		}}...)
	}
	for i, endpoint := range endpoints {
		endpoint.LoadBalancedEndpointSetName = endpoint.Name
		endpoint.LoadBalancerProbe = &gwacl.LoadBalancerProbe{
			Port:     endpoint.Port,
			Protocol: "TCP",
		}
		endpoints[i] = endpoint
	}
	return endpoints
}

// newRole creates a gwacl.Role object (an Azure Virtual Machine) which uses
// the given Virtual Hard Drive.
//
// The VM will have:
// - an 'ubuntu' user defined with an unguessable (randomly generated) password
// - its ssh port (TCP 22) open
// (if a state server)
// - its state port (TCP mongoDB) port open
// - its API port (TCP) open
//
// roleSize is the name of one of Azure's machine types, e.g. ExtraSmall,
// Large, A6 etc.
func (env *azureEnviron) newRole(roleSize string, vhd *gwacl.OSVirtualHardDisk, userData string, stateServer bool) *gwacl.Role {
	roleName := gwacl.MakeRandomRoleName("juju")
	// Create a Linux Configuration with the username and the password
	// empty and disable SSH with password authentication.
	hostname := roleName
	username := "ubuntu"
	password := gwacl.MakeRandomPassword()
	linuxConfigurationSet := gwacl.NewLinuxProvisioningConfigurationSet(hostname, username, password, userData, "true")
	// Generate a Network Configuration with the initially required ports open.
	networkConfigurationSet := gwacl.NewNetworkConfigurationSet(env.getInitialEndpoints(stateServer), nil)
	role := gwacl.NewRole(
		roleSize, roleName, vhd,
		[]gwacl.ConfigurationSet{*linuxConfigurationSet, *networkConfigurationSet},
	)
	role.AvailabilitySetName = "juju"
	return role
}

// StopInstances is specified in the InstanceBroker interface.
func (env *azureEnviron) StopInstances(ids ...instance.Id) error {
	context, err := env.getManagementAPI()
	if err != nil {
		return err
	}
	defer env.releaseManagementAPI(context)

	// Map services to role names we want to delete.
	serviceInstances := make(map[string]map[string]bool)
	var serviceNames []string
	for _, id := range ids {
		serviceName, roleName := env.splitInstanceId(id)
		if roleName == "" {
			serviceInstances[serviceName] = nil
			serviceNames = append(serviceNames, serviceName)
		} else {
			deleteRoleNames, ok := serviceInstances[serviceName]
			if !ok {
				deleteRoleNames = make(map[string]bool)
				serviceInstances[serviceName] = deleteRoleNames
				serviceNames = append(serviceNames, serviceName)
			}
			deleteRoleNames[roleName] = true
		}
	}

	// Load the properties of each service, so we know whether to
	// delete the entire service.
	//
	// Note: concurrent operations on Affinity Groups have been
	// found to cause conflict responses, so we do everything serially.
	for _, serviceName := range serviceNames {
		deleteRoleNames := serviceInstances[serviceName]
		service, err := context.GetHostedServiceProperties(serviceName, true)
		if err != nil {
			return err
		} else if len(service.Deployments) != 1 {
			continue
		}
		// Filter the instances that have no corresponding role.
		var roleNames set.Strings
		for _, role := range service.Deployments[0].RoleList {
			roleNames.Add(role.RoleName)
		}
		for roleName := range deleteRoleNames {
			if !roleNames.Contains(roleName) {
				delete(deleteRoleNames, roleName)
			}
		}
		// If we're deleting all the roles, we need to delete the
		// entire cloud service or we'll get an error. deleteRoleNames
		// is nil if we're dealing with a legacy deployment.
		if deleteRoleNames == nil || len(deleteRoleNames) == roleNames.Size() {
			if err := context.DeleteHostedService(serviceName); err != nil {
				return err
			}
		} else {
			for roleName := range deleteRoleNames {
				if err := context.DeleteRole(&gwacl.DeleteRoleRequest{
					ServiceName:    serviceName,
					DeploymentName: service.Deployments[0].Name,
					RoleName:       roleName,
					DeleteMedia:    true,
				}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// destroyAllServices destroys all Cloud Services and deployments contained.
// This is needed to clean up broken environments, in which there are cloud
// services with no deployments.
func (env *azureEnviron) destroyAllServices() error {
	context, err := env.getManagementAPI()
	if err != nil {
		return err
	}
	defer env.releaseManagementAPI(context)

	request := &gwacl.ListPrefixedHostedServicesRequest{ServiceNamePrefix: env.getEnvPrefix()}
	services, err := context.ListPrefixedHostedServices(request)
	if err != nil {
		return err
	}
	for _, service := range services {
		if err := context.DeleteHostedService(service.ServiceName); err != nil {
			return err
		}
	}
	return nil
}

// splitInstanceId splits the specified instance.Id into its
// cloud-service and role parts. Both values will be empty
// if the instance-id is non-matching, and role will be empty
// for legacy instance-ids.
func (env *azureEnviron) splitInstanceId(id instance.Id) (service, role string) {
	prefix := env.getEnvPrefix()
	if !strings.HasPrefix(string(id), prefix) {
		return "", ""
	}
	fields := strings.Split(string(id)[len(prefix):], "-")
	service = prefix + fields[0]
	if len(fields) > 1 {
		role = fields[1]
	}
	return service, role
}

// Instances is specified in the Environ interface.
func (env *azureEnviron) Instances(ids []instance.Id) ([]instance.Instance, error) {
	context, err := env.getManagementAPI()
	if err != nil {
		return nil, err
	}
	defer env.releaseManagementAPI(context)

	type instanceId struct {
		serviceName, roleName string
	}

	instancesIds := make([]instanceId, len(ids))
	var serviceNames set.Strings
	for i, id := range ids {
		serviceName, roleName := env.splitInstanceId(id)
		if serviceName == "" {
			continue
		}
		instancesIds[i] = instanceId{
			serviceName: serviceName,
			roleName:    roleName,
		}
		serviceNames.Add(serviceName)
	}

	// Map service names to gwacl.HostedServices.
	services, err := context.ListSpecificHostedServices(&gwacl.ListSpecificHostedServicesRequest{
		ServiceNames: serviceNames.Values(),
	})
	if err != nil {
		return nil, err
	}
	if len(services) == 0 {
		return nil, environs.ErrNoInstances
	}
	hostedServices := make(map[string]*gwacl.HostedService)
	for _, s := range services {
		hostedService, err := context.GetHostedServiceProperties(s.ServiceName, true)
		if err != nil {
			return nil, err
		}
		hostedServices[s.ServiceName] = hostedService
	}

	err = nil
	instances := make([]instance.Instance, len(ids))
	for i, id := range instancesIds {
		if id.serviceName == "" {
			// Previously determined to be an invalid instance ID.
			continue
		}
		hostedService := hostedServices[id.serviceName]
		instance, err := env.getInstance(hostedService, id.roleName)
		if err == nil {
			instances[i] = instance
		} else {
			logger.Debugf("failed to get instance for role %q in service %q: %v", id.roleName, hostedService.ServiceName, err)
		}
	}
	for _, instance := range instances {
		if instance == nil {
			err = environs.ErrPartialInstances
		}
	}
	return instances, err
}

// AllocateAddress requests a new address to be allocated for the
// given instance on the given network. This is not implemented on the
// Azure provider yet.
func (*azureEnviron) AllocateAddress(_ instance.Id, _ network.Id) (network.Address, error) {
	return network.Address{}, errors.NotImplementedf("AllocateAddress")
}

// ListNetworks returns basic information about all networks known
// by the provider for the environment. They may be unknown to juju
// yet (i.e. when called initially or when a new network was created).
// This is not implemented by the Azure provider yet.
func (*azureEnviron) ListNetworks() ([]network.BasicInfo, error) {
	return nil, errors.NotImplementedf("ListNetworks")
}

// AllInstances is specified in the InstanceBroker interface.
func (env *azureEnviron) AllInstances() ([]instance.Instance, error) {
	// The instance list is built using the list of all the Azure
	// Services (instance==service).
	// Acquire management API object.
	context, err := env.getManagementAPI()
	if err != nil {
		return nil, err
	}
	defer env.releaseManagementAPI(context)

	request := &gwacl.ListPrefixedHostedServicesRequest{ServiceNamePrefix: env.getEnvPrefix()}
	serviceDescriptors, err := context.ListPrefixedHostedServices(request)
	if err != nil {
		return nil, err
	}

	var instances []instance.Instance
	for _, sd := range serviceDescriptors {
		hostedService, err := context.GetHostedServiceProperties(sd.ServiceName, true)
		if err != nil {
			return nil, err
		} else if len(hostedService.Deployments) != 1 {
			continue
		}
		deployment := &hostedService.Deployments[0]
		for _, role := range deployment.RoleList {
			instance, err := env.getInstance(hostedService, role.RoleName)
			if err != nil {
				return nil, err
			}
			instances = append(instances, instance)
		}
	}
	return instances, nil
}

// getEnvPrefix returns the prefix used to name the objects specific to this
// environment.
func (env *azureEnviron) getEnvPrefix() string {
	return fmt.Sprintf("juju-%s-", env.Config().Name())
}

// Storage is specified in the Environ interface.
func (env *azureEnviron) Storage() storage.Storage {
	return env.getSnapshot().storage
}

// Destroy is specified in the Environ interface.
func (env *azureEnviron) Destroy() error {
	logger.Debugf("destroying environment %q", env.Config().Name())

	// Stop all instances.
	if err := env.destroyAllServices(); err != nil {
		return fmt.Errorf("cannot destroy instances: %v", err)
	}

	// Delete vnet and affinity group. Deleting the virtual network
	// may fail for inexplicable reasons (cannot delete in the Azure
	// console either for some amount of time after deleting dependent
	// VMs), so we only treat this as a warning. There is no cost
	// associated with a vnet or affinity group.
	if err := env.deleteVirtualNetwork(); err != nil {
		logger.Warningf("cannot delete the environment's virtual network: %v", err)
	}
	if err := env.deleteAffinityGroup(); err != nil {
		logger.Warningf("cannot delete the environment's affinity group: %v", err)
	}

	// Delete storage.
	// Deleting the storage is done last so that if something fails
	// half way through the Destroy() method, the storage won't be cleaned
	// up and thus an attempt to re-boostrap the environment will lead to
	// a "error: environment is already bootstrapped" error.
	if err := env.Storage().RemoveAll(); err != nil {
		return fmt.Errorf("cannot clean up storage: %v", err)
	}
	return nil
}

// OpenPorts is specified in the Environ interface. However, Azure does not
// support the global firewall mode.
func (env *azureEnviron) OpenPorts(ports []network.PortRange) error {
	return nil
}

// ClosePorts is specified in the Environ interface. However, Azure does not
// support the global firewall mode.
func (env *azureEnviron) ClosePorts(ports []network.PortRange) error {
	return nil
}

// Ports is specified in the Environ interface.
func (env *azureEnviron) Ports() ([]network.PortRange, error) {
	// TODO: implement this.
	return []network.PortRange{}, nil
}

// Provider is specified in the Environ interface.
func (env *azureEnviron) Provider() environs.EnvironProvider {
	return azureEnvironProvider{}
}

// azureManagementContext wraps two things: a gwacl.ManagementAPI (effectively
// a session on the Azure management API) and a tempCertFile, which keeps track
// of the temporary certificate file that needs to be deleted once we're done
// with this particular session.
// Since it embeds *gwacl.ManagementAPI, you can use it much as if it were a
// pointer to a ManagementAPI object.  Just don't forget to release it after
// use.
type azureManagementContext struct {
	*gwacl.ManagementAPI
	certFile *tempCertFile
}

var (
	retryPolicy = gwacl.RetryPolicy{
		NbRetries: 6,
		HttpStatusCodes: []int{
			http.StatusConflict,
			http.StatusRequestTimeout,
			http.StatusInternalServerError,
			http.StatusServiceUnavailable,
		},
		Delay: 10 * time.Second}
)

// getManagementAPI obtains a context object for interfacing with Azure's
// management API.
// For now, each invocation just returns a separate object.  This is probably
// wasteful (each context gets its own SSL connection) and may need optimizing
// later.
func (env *azureEnviron) getManagementAPI() (*azureManagementContext, error) {
	snap := env.getSnapshot()
	subscription := snap.ecfg.managementSubscriptionId()
	certData := snap.ecfg.managementCertificate()
	certFile, err := newTempCertFile([]byte(certData))
	if err != nil {
		return nil, err
	}
	// After this point, if we need to leave prematurely, we should clean
	// up that certificate file.
	location := snap.ecfg.location()
	mgtAPI, err := gwacl.NewManagementAPIWithRetryPolicy(subscription, certFile.Path(), location, retryPolicy)
	if err != nil {
		certFile.Delete()
		return nil, err
	}
	context := azureManagementContext{
		ManagementAPI: mgtAPI,
		certFile:      certFile,
	}
	return &context, nil
}

// releaseManagementAPI frees up a context object obtained through
// getManagementAPI.
func (env *azureEnviron) releaseManagementAPI(context *azureManagementContext) {
	// Be tolerant to incomplete context objects, in case we ever get
	// called during cleanup of a failed attempt to create one.
	if context == nil || context.certFile == nil {
		return
	}
	// For now, all that needs doing is to delete the temporary certificate
	// file.  We may do cleverer things later, such as connection pooling
	// where this method returns a context to the pool.
	context.certFile.Delete()
}

// updateStorageAccountKey queries the storage account key, and updates the
// version cached in env.storageAccountKey.
//
// It takes a snapshot in order to preserve transactional integrity relative
// to the snapshot's starting state, without having to lock the environment
// for the duration.  If there is a conflicting change to env relative to the
// state recorded in the snapshot, this function will fail.
func (env *azureEnviron) updateStorageAccountKey(snapshot *azureEnviron) (string, error) {
	// This method follows an RCU pattern, an optimistic technique to
	// implement atomic read-update transactions: get a consistent snapshot
	// of state; process data; enter critical section; check for conflicts;
	// write back changes.  The advantage is that there are no long-held
	// locks, in particular while waiting for the request to Azure to
	// complete.
	// "Get a consistent snapshot of state" is the caller's responsibility.
	// The caller can use env.getSnapshot().

	// Process data: get a current account key from Azure.
	key, err := env.queryStorageAccountKey()
	if err != nil {
		return "", err
	}

	// Enter critical section.
	env.Lock()
	defer env.Unlock()

	// Check for conflicts: is the config still what it was?
	if env.ecfg != snapshot.ecfg {
		// The environment has been reconfigured while we were
		// working on this, so the key we just get may not be
		// appropriate any longer.  So fail.
		// Whatever we were doing isn't likely to be right any more
		// anyway.  Otherwise, it might be worth returning the key
		// just in case it still works, and proceed without updating
		// env.storageAccountKey.
		return "", fmt.Errorf("environment was reconfigured")
	}

	// Write back changes.
	env.storageAccountKey = key
	return key, nil
}

// getStorageContext obtains a context object for interfacing with Azure's
// storage API.
// For now, each invocation just returns a separate object.  This is probably
// wasteful (each context gets its own SSL connection) and may need optimizing
// later.
func (env *azureEnviron) getStorageContext() (*gwacl.StorageContext, error) {
	snap := env.getSnapshot()
	key := snap.storageAccountKey
	if key == "" {
		// We don't know the storage-account key yet.  Request it.
		var err error
		key, err = env.updateStorageAccountKey(snap)
		if err != nil {
			return nil, err
		}
	}
	context := gwacl.StorageContext{
		Account:       snap.ecfg.storageAccountName(),
		Key:           key,
		AzureEndpoint: gwacl.GetEndpoint(snap.ecfg.location()),
		RetryPolicy:   retryPolicy,
	}
	return &context, nil
}

// Region is specified in the HasRegion interface.
func (env *azureEnviron) Region() (simplestreams.CloudSpec, error) {
	ecfg := env.getSnapshot().ecfg
	return simplestreams.CloudSpec{
		Region:   ecfg.location(),
		Endpoint: string(gwacl.GetEndpoint(ecfg.location())),
	}, nil
}

// SupportsUnitPlacement is specified in the state.EnvironCapability interface.
func (env *azureEnviron) SupportsUnitPlacement() error {
	if env.getSnapshot().ecfg.availabilitySetsEnabled() {
		return fmt.Errorf("unit placement is not supported with availability-sets-enabled")
	}
	return nil
}
