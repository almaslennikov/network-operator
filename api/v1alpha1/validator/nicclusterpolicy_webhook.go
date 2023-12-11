/*
2023 NVIDIA CORPORATION & AFFILIATES

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

package validator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"github.com/containers/image/v5/docker/reference"
	"github.com/xeipuuv/gojsonschema"
	"golang.org/x/exp/slices"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/Mellanox/network-operator/api/v1alpha1"
	"github.com/Mellanox/network-operator/pkg/config"
	"github.com/Mellanox/network-operator/pkg/state"
)

const (
	fqdnRegex              = `^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z]{2,})+$`
	sriovResourceNameRegex = `^([A-Za-z0-9][A-Za-z0-9_.]*)?[A-Za-z0-9]$`
	rdmaResourceNameRegex  = `^([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$`
)

// log is for logging in this package.
var nicClusterPolicyLog = logf.Log.WithName("nicclusterpolicy-resource")

var schemaValidators *schemaValidator

var skipValidations = false

var envConfig = config.FromEnv().State

type nicClusterPolicyValidator struct {
	v1alpha1.NicClusterPolicy
}

type devicePluginSpecWrapper struct {
	v1alpha1.DevicePluginSpec
}

type ibKubernetesSpecWrapper struct {
	v1alpha1.IBKubernetesSpec
}

type ofedDriverSpecWrapper struct {
	v1alpha1.OFEDDriverSpec
}

func SetupNicClusterPolicyWebhookWithManager(mgr ctrl.Manager) error {
	nicClusterPolicyLog.Info("Nic cluster policy webhook admission controller")
	InitSchemaValidator("./webhook-schemas")
	return ctrl.NewWebhookManagedBy(mgr).
		For(&v1alpha1.NicClusterPolicy{}).
		WithValidator(&nicClusterPolicyValidator{}).
		Complete()
}

//nolint:lll
//+kubebuilder:webhook:path=/validate-mellanox-com-v1alpha1-nicclusterpolicy,mutating=false,failurePolicy=fail,sideEffects=None,groups=mellanox.com,resources=nicclusterpolicies,verbs=create;update,versions=v1alpha1,name=vnicclusterpolicy.kb.io,admissionReviewVersions=v1

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (w *nicClusterPolicyValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	if skipValidations {
		nicClusterPolicyLog.Info("skipping CR validation")
		return nil, nil
	}

	nicClusterPolicy, ok := obj.(*v1alpha1.NicClusterPolicy)
	if !ok {
		return nil, errors.New("failed to unmarshal NicClusterPolicy object to validate")
	}
	w.NicClusterPolicy = *nicClusterPolicy
	nicClusterPolicyLog.Info("validate create", "name", w.Name)
	return nil, w.validateNicClusterPolicy()
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (w *nicClusterPolicyValidator) ValidateUpdate(
	_ context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	if skipValidations {
		nicClusterPolicyLog.Info("skipping CR validation")
		return nil, nil
	}

	nicClusterPolicy, ok := newObj.(*v1alpha1.NicClusterPolicy)
	if !ok {
		return nil, errors.New("failed to unmarshal NicClusterPolicy object to validate")
	}
	w.NicClusterPolicy = *nicClusterPolicy
	nicClusterPolicyLog.Info("validate update", "name", w.Name)
	return nil, w.validateNicClusterPolicy()
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (w *nicClusterPolicyValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	if skipValidations {
		nicClusterPolicyLog.Info("skipping CR validation")
		return nil, nil
	}

	nicClusterPolicyLog.Info("validate delete", "name", w.Name)

	// Validation for delete call is not required
	return nil, nil
}

/*
We are validating here NicClusterPolicy:
 1. IBKubernetes.pKeyGUIDPoolRangeStart and IBKubernetes.pKeyGUIDPoolRangeEnd must be valid GUID and valid range.
 2. OFEDDriver driver configuration
    2.1 version must be a valid ofed version.
    2.2 safeLoad feature can be enabled only when autoUpgrade is enabled
 3. RdmaSharedDevicePlugin.Config.
    3.1. Configuration is a valid JSON and check its schema.
    3.2. resourceName is valid for k8s.
    3.3. At least one of the supported selectors exists.
    3.4. All selectors are strings.
 4. SriovNetworkDevicePlugin.Config.
    4.1. Configuration is a valid JSON and check its schema.
    4.2. resourceName is valid for k8s.
    4.3. At least one of the supported selectors exists.
    4.4. All selectors are strings.
*/
func (w *nicClusterPolicyValidator) validateNicClusterPolicy() error {
	var allErrs field.ErrorList
	// Validate Repository
	allErrs = w.validateRepositories(allErrs)
	allErrs = w.validateContainerResources(&w.NicClusterPolicy, allErrs)
	// Validate IBKubernetes
	ibKubernetes := w.Spec.IBKubernetes
	if ibKubernetes != nil {
		wrapper := ibKubernetesSpecWrapper{IBKubernetesSpec: *w.Spec.IBKubernetes}
		allErrs = append(allErrs, wrapper.validate(field.NewPath("spec").Child("ibKubernetes"))...)
	}
	// Validate OFEDDriverSpec
	ofedDriver := w.Spec.OFEDDriver
	if ofedDriver != nil {
		wrapper := ofedDriverSpecWrapper{OFEDDriverSpec: *w.Spec.OFEDDriver}
		ofedDriverFieldPath := field.NewPath("spec").Child("ofedDriver")
		allErrs = append(append(allErrs,
			wrapper.validateVersion(ofedDriverFieldPath)...),
			wrapper.validateSafeLoad(ofedDriverFieldPath)...)
	}
	// Validate RdmaSharedDevicePlugin
	rdmaSharedDevicePlugin := w.Spec.RdmaSharedDevicePlugin
	if rdmaSharedDevicePlugin != nil {
		wrapper := devicePluginSpecWrapper{DevicePluginSpec: *w.Spec.RdmaSharedDevicePlugin}
		allErrs = append(allErrs, wrapper.validateRdmaSharedDevicePlugin(
			field.NewPath("spec").Child("rdmaSharedDevicePlugin"))...)
	}
	// Validate SriovDevicePlugin
	sriovNetworkDevicePlugin := w.Spec.SriovDevicePlugin
	if sriovNetworkDevicePlugin != nil {
		wrapper := devicePluginSpecWrapper{DevicePluginSpec: *w.Spec.SriovDevicePlugin}
		allErrs = append(allErrs, wrapper.validateSriovNetworkDevicePlugin(
			field.NewPath("spec").Child("sriovNetworkDevicePlugin"))...)
	}

	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: "mellanox.com", Kind: "NicClusterPolicy"},
		w.Name, allErrs)
}
func (dp *devicePluginSpecWrapper) validateSriovNetworkDevicePlugin(fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	var sriovNetworkDevicePluginConfigJSON map[string]interface{}
	sriovNetworkDevicePluginConfig := *dp.Config

	// Validate if the SRIOV Network Device Plugin Config is a valid json
	if err := json.Unmarshal([]byte(sriovNetworkDevicePluginConfig), &sriovNetworkDevicePluginConfigJSON); err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("Config"), dp.Config,
			"Invalid json of SriovNetworkDevicePluginConfig"))
		return allErrs
	}

	// Load the JSON Schema
	sriovNetworkDevicePluginSchema, err := schemaValidators.GetSchema("sriov_network_device_plugin")
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("Config"), dp.Config,
			"Invalid json schema "+err.Error()))
		return allErrs
	}
	acceleratorJSONSchema, err := schemaValidators.GetSchema("accelerator_selector")
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("Config"), dp.Config,
			"Invalid json schema "+err.Error()))
		return allErrs
	}
	netDeviceJSONSchema, err := schemaValidators.GetSchema("net_device")
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("Config"), dp.Config,
			"Invalid json schema "+err.Error()))
		return allErrs
	}
	auxNetDeviceJSONSchema, err := schemaValidators.GetSchema("aux_net_device")
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("Config"), dp.Config,
			"Invalid json schema "+err.Error()))
		return allErrs
	}

	// Load the Sriov Network Device Plugin JSON Loader
	sriovNetworkDevicePluginConfigJSONLoader := gojsonschema.NewStringLoader(sriovNetworkDevicePluginConfig)

	// Perform schema validation
	result, err := sriovNetworkDevicePluginSchema.Validate(sriovNetworkDevicePluginConfigJSONLoader)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("Config"), dp.Config,
			"Invalid json configuration of SriovNetworkDevicePluginConfig"+err.Error()))
		return allErrs
	} else if !result.Valid() {
		for _, ResultErr := range result.Errors() {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("Config"), dp.Config, ResultErr.Description()))
		}
		return allErrs
	}
	if resourceListInterface := sriovNetworkDevicePluginConfigJSON["resourceList"]; resourceListInterface != nil {
		resourceList, _ := resourceListInterface.([]interface{})
		for _, resourceInterface := range resourceList {
			resource := resourceInterface.(map[string]interface{})
			resourceJSONString, _ := json.Marshal(resource)
			resourceJSONLoader := gojsonschema.NewStringLoader(string(resourceJSONString))
			var selectorResult *gojsonschema.Result
			var selectorErr error
			var ok bool
			ok, allErrs = validateResourceNamePrefix(resource, allErrs, fldPath, dp)
			if !ok {
				return allErrs
			}
			deviceType := resource["deviceType"]
			switch deviceType {
			case "accelerator":
				selectorResult, selectorErr = acceleratorJSONSchema.Validate(resourceJSONLoader)
			case "auxNetDevice":
				selectorResult, selectorErr = auxNetDeviceJSONSchema.Validate(resourceJSONLoader)
			default:
				selectorResult, selectorErr = netDeviceJSONSchema.Validate(resourceJSONLoader)
			}
			if selectorErr != nil {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("Config"), dp.Config,
					selectorErr.Error()))
			} else if !selectorResult.Valid() {
				for _, selectorResultErr := range selectorResult.Errors() {
					allErrs = append(allErrs, field.Invalid(fldPath.Child("Config"), dp.Config,
						selectorResultErr.Description()))
				}
			}
		}
	}
	return allErrs
}

func validateResourceNamePrefix(resource map[string]interface{},
	allErrs field.ErrorList, fldPath *field.Path, dp *devicePluginSpecWrapper) (bool, field.ErrorList) {
	resourceName := resource["resourceName"].(string)
	if !isValidSriovNetworkDevicePluginResourceName(resourceName) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("Config"), dp.Config,
			"Invalid Resource name, it must consist of alphanumeric characters, '_' or '.', "+
				"and must start and end with an alphanumeric character (e.g. 'MyName',  or 'my.name',  "+
				"or '123_abc', regex used for validation is "+sriovResourceNameRegex))
		return false, allErrs
	}
	resourcePrefix, ok := resource["resourcePrefix"]
	if ok {
		if !isValidFQDN(resourcePrefix.(string)) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("Config"), dp.Config,
				"Invalid Resource prefix, it must be a valid FQDN"+
					"regex used for validation is "+fqdnRegex))
			return false, allErrs
		}
	}
	return true, allErrs
}

func (dp *devicePluginSpecWrapper) validateRdmaSharedDevicePlugin(fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	var rdmaSharedDevicePluginConfigJSON map[string]interface{}
	rdmaSharedDevicePluginConfig := *dp.Config

	// Validate if the RDMA Shared Device Plugin Config is a valid json
	if err := json.Unmarshal([]byte(rdmaSharedDevicePluginConfig), &rdmaSharedDevicePluginConfigJSON); err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("Config"),
			dp.Config, "Invalid json of RdmaSharedDevicePluginConfig"+err.Error()))
		return allErrs
	}

	// Perform schema validation
	rdmaSharedDevicePluginSchema, err := schemaValidators.GetSchema("rdma_shared_device_plugin")
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("Config"), dp.Config,
			"Invalid json schema "+err.Error()))
		return allErrs
	}
	rdmaSharedDevicePluginConfigJSONLoader := gojsonschema.NewStringLoader(rdmaSharedDevicePluginConfig)
	result, err := rdmaSharedDevicePluginSchema.Validate(rdmaSharedDevicePluginConfigJSONLoader)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("Config"), dp.Config,
			"Invalid json of RdmaSharedDevicePluginConfig"+err.Error()))
	} else if result.Valid() {
		configListInterface := rdmaSharedDevicePluginConfigJSON["configList"]
		configList, _ := configListInterface.([]interface{})
		for _, configInterface := range configList {
			dpConfig := configInterface.(map[string]interface{})
			resourceName := dpConfig["resourceName"].(string)
			if !isValidRdmaSharedDevicePluginResourceName(resourceName) {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("Config"),
					dp.Config, "Invalid Resource name, it must consist of alphanumeric characters, "+
						"'-', '_' or '.', and must start and end with an alphanumeric character "+
						"(e.g. 'MyName',  or 'my.name',  or '123-abc') regex used for validation is "+rdmaResourceNameRegex))
			}
			resourcePrefix, ok := dpConfig["resourcePrefix"]
			if ok {
				if !isValidFQDN(resourcePrefix.(string)) {
					allErrs = append(allErrs, field.Invalid(fldPath.Child("Config"), dp.Config,
						"Invalid Resource prefix, it must be a valid FQDN "+
							"regex used for validation is "+fqdnRegex))
					return allErrs
				}
			}
		}
	} else {
		for _, ResultErr := range result.Errors() {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("Config"), dp.Config, ResultErr.Description()))
		}
	}
	return allErrs
}

// validate is a helper function to perform validation for IBKubernetesSpec.
func (ibk *ibKubernetesSpecWrapper) validate(fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	if !isValidPKeyGUID(ibk.PKeyGUIDPoolRangeStart) || !isValidPKeyGUID(ibk.PKeyGUIDPoolRangeEnd) {
		if !isValidPKeyGUID(ibk.PKeyGUIDPoolRangeStart) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("pKeyGUIDPoolRangeStart"),
				ibk.PKeyGUIDPoolRangeStart, "pKeyGUIDPoolRangeStart must be a valid GUID format:"+
					"xx:xx:xx:xx:xx:xx:xx:xx with Hexa numbers"))
		}
		if !isValidPKeyGUID(ibk.PKeyGUIDPoolRangeEnd) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("pKeyGUIDPoolRangeEnd"),
				ibk.PKeyGUIDPoolRangeEnd, "pKeyGUIDPoolRangeEnd must be a valid GUID format: "+
					"xx:xx:xx:xx:xx:xx:xx:xx with Hexa numbers"))
		}
		return allErrs
	} else if !isValidPKeyRange(ibk.PKeyGUIDPoolRangeStart, ibk.PKeyGUIDPoolRangeEnd) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("pKeyGUIDPoolRangeEnd"),
			ibk.PKeyGUIDPoolRangeEnd, "pKeyGUIDPoolRangeStart-pKeyGUIDPoolRangeEnd must be a valid range"))
	}
	return allErrs
}

// isValidPKeyGUID checks if a given string is a valid GUID format.
func isValidPKeyGUID(guid string) bool {
	PKeyGUIDPattern := `^([0-9A-Fa-f]{2}:){7}([0-9A-Fa-f]{2})$`
	PKeyGUIDRegex := regexp.MustCompile(PKeyGUIDPattern)
	return PKeyGUIDRegex.MatchString(guid)
}

// isValidPKeyRange checks if range of startGUID and endGUID sis valid
func isValidPKeyRange(startGUID, endGUID string) bool {
	startGUIDWithoutSeparator := strings.ReplaceAll(startGUID, ":", "")
	endGUIDWithoutSeparator := strings.ReplaceAll(endGUID, ":", "")

	startGUIDIntValue := new(big.Int)
	endGUIDIntValue := new(big.Int)
	startGUIDIntValue, _ = startGUIDIntValue.SetString(startGUIDWithoutSeparator, 16)
	endGUIDIntValue, _ = endGUIDIntValue.SetString(endGUIDWithoutSeparator, 16)
	return endGUIDIntValue.Cmp(startGUIDIntValue) > 0
}

func (ofedSpec *ofedDriverSpecWrapper) validateVersion(fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	// Perform version validation logic here
	if !isValidOFEDVersion(ofedSpec.Version) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("version"), ofedSpec.Version,
			`invalid OFED version, the regex used for validation is ^(\d+\.\d+-\d+(\.\d+)*)$ `))
	}
	return allErrs
}

func (ofedSpec *ofedDriverSpecWrapper) validateSafeLoad(fldPath *field.Path) field.ErrorList {
	upgradePolicy := ofedSpec.OfedUpgradePolicy
	if upgradePolicy == nil {
		return nil
	}
	if !upgradePolicy.SafeLoad {
		return nil
	}
	allErrs := field.ErrorList{}
	upgradePolicyFieldPath := fldPath.Child("upgradePolicy")
	if !upgradePolicy.AutoUpgrade {
		allErrs = append(allErrs, field.Forbidden(upgradePolicyFieldPath.Child("safeLoad"),
			fmt.Sprintf("safeLoad requires %s to be true",
				upgradePolicyFieldPath.Child("autoUpgrade").String())))
	}
	return allErrs
}

func (w *nicClusterPolicyValidator) validateRepositories(allErrs field.ErrorList) field.ErrorList {
	fp := field.NewPath("spec")
	if w.Spec.OFEDDriver != nil {
		allErrs = validateRepository(w.Spec.OFEDDriver.ImageSpec.Repository, allErrs, fp, "nicFeatureDiscovery")
	}
	if w.Spec.RdmaSharedDevicePlugin != nil {
		allErrs = validateRepository(w.Spec.RdmaSharedDevicePlugin.ImageSpec.Repository,
			allErrs, fp, "rdmaSharedDevicePlugin")
	}
	if w.Spec.SriovDevicePlugin != nil {
		allErrs = validateRepository(w.Spec.SriovDevicePlugin.ImageSpec.Repository, allErrs, fp, "sriovDevicePlugin")
	}
	if w.Spec.IBKubernetes != nil {
		allErrs = validateRepository(w.Spec.IBKubernetes.ImageSpec.Repository, allErrs, fp, "ibKubernetes")
	}
	if w.Spec.NvIpam != nil {
		allErrs = validateRepository(w.Spec.NvIpam.ImageSpec.Repository, allErrs, fp, "nvIpam")
	}
	if w.Spec.NicFeatureDiscovery != nil {
		allErrs = validateRepository(w.Spec.NicFeatureDiscovery.ImageSpec.Repository, allErrs, fp, "nicFeatureDiscovery")
	}
	if w.Spec.SecondaryNetwork != nil {
		snfp := fp.Child("secondaryNetwork")
		if w.Spec.SecondaryNetwork.CniPlugins != nil {
			allErrs = validateRepository(w.Spec.SecondaryNetwork.CniPlugins.Repository, allErrs, snfp, "cniPlugins")
		}
		if w.Spec.SecondaryNetwork.IPoIB != nil {
			allErrs = validateRepository(w.Spec.SecondaryNetwork.IPoIB.Repository, allErrs, snfp, "ipoib")
		}
		if w.Spec.SecondaryNetwork.Multus != nil {
			allErrs = validateRepository(w.Spec.SecondaryNetwork.Multus.Repository, allErrs, snfp, "multus")
		}
		if w.Spec.SecondaryNetwork.IpamPlugin != nil {
			allErrs = validateRepository(w.Spec.SecondaryNetwork.IpamPlugin.Repository, allErrs, snfp, "ipamPlugin")
		}
	}
	return allErrs
}

func validateRepository(repo string, allErrs field.ErrorList, fp *field.Path, child string) field.ErrorList {
	_, err := reference.ParseNormalizedNamed(repo)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fp.Child(child).Child("repository"),
			repo, "invalid container image repository format"))
	}
	return allErrs
}

type newStateFunc func(
	k8sAPIClient client.Client, scheme *runtime.Scheme, manifestDir string) (state.State, state.ManifestRenderer, error)
type stateRenderData struct {
	spec        interface{}
	newState    newStateFunc
	manifestDir string
}

func (w *nicClusterPolicyValidator) validateContainerResources(
	policy *v1alpha1.NicClusterPolicy, allErrs field.ErrorList) field.ErrorList {
	fp := field.NewPath("spec")

	manifestBaseDir := envConfig.ManifestBaseDir

	states := map[string]stateRenderData{
		"ofedDriver": {
			w.Spec.OFEDDriver, state.NewStateOFED,
			filepath.Join(manifestBaseDir, "state-ofed-driver"),
		},
		"rdmaSharedDevicePlugin": {
			w.Spec.RdmaSharedDevicePlugin, state.NewStateSharedDp,
			filepath.Join(manifestBaseDir, "state-rdma-device-plugin"),
		},
		"sriovDevicePlugin": {
			w.Spec.SriovDevicePlugin, state.NewStateSriovDp,
			filepath.Join(manifestBaseDir, "state-sriov-device-plugin"),
		},
		"ibKubernetes": {
			w.Spec.IBKubernetes, state.NewStateIBKubernetes,
			filepath.Join(manifestBaseDir, "state-ib-kubernetes"),
		},
		"nvIpam": {
			w.Spec.NvIpam, state.NewStateNVIPAMCNI,
			filepath.Join(manifestBaseDir, "state-nv-ipam-cni"),
		},
		"nicFeatureDiscovery": {
			w.Spec.NicFeatureDiscovery, state.NewStateNICFeatureDiscovery,
			filepath.Join(manifestBaseDir, "state-nic-feature-discovery"),
		},
	}
	for stateName, renderData := range states {
		localData := renderData
		allErrs = validateContainerResourcesIfNotNil(&localData, policy, allErrs, fp, stateName)
	}

	if w.Spec.SecondaryNetwork != nil {
		snfp := fp.Child("secondaryNetwork")

		states := map[string]stateRenderData{
			"cniPlugins": {
				w.Spec.SecondaryNetwork.CniPlugins, state.NewStateCNIPlugins,
				filepath.Join(manifestBaseDir, "state-container-networking-plugins"),
			},
			"ipoib": {
				w.Spec.SecondaryNetwork.IPoIB, state.NewStateIPoIBCNI,
				filepath.Join(manifestBaseDir, "state-ipoib-cni"),
			},
			"multus": {
				w.Spec.SecondaryNetwork.Multus, state.NewStateMultusCNI,
				filepath.Join(manifestBaseDir, "state-multus-cni"),
			},
			"ipamPlugin": {
				w.Spec.SecondaryNetwork.IpamPlugin, state.NewStateWhereaboutsCNI,
				filepath.Join(manifestBaseDir, "state-whereabouts-cni"),
			},
		}
		for stateName, renderData := range states {
			localData := renderData
			allErrs = validateContainerResourcesIfNotNil(&localData, policy, allErrs, snfp, stateName)
		}
	}
	return allErrs
}

func validateContainerResourcesIfNotNil(
	resource *stateRenderData, policy *v1alpha1.NicClusterPolicy, allErrs field.ErrorList,
	fp *field.Path, fieldName string) field.ErrorList {
	if resource.spec == nil {
		return allErrs
	}

	// Use reflection to check if ContainerResources is nil
	val := reflect.ValueOf(resource.spec)
	if val.Kind() == reflect.Ptr && !val.IsNil() {
		imageSpec := val.Elem().FieldByName("ImageSpec")
		if imageSpec.IsValid() {
			containerResources := imageSpec.FieldByName("ContainerResources")
			if containerResources.IsValid() && !containerResources.IsNil() {
				_, renderer, err := resource.newState(nil, nil, resource.manifestDir)
				if err != nil {
					nicClusterPolicyLog.Error(err, "failed to created state renderer")
				}
				return validateResourceRequirements(
					containerResources.Interface().([]v1alpha1.ResourceRequirements),
					policy, renderer, allErrs, fp, fieldName)
			}
		}
	}

	return allErrs
}

func validateResourceRequirements(resources []v1alpha1.ResourceRequirements,
	policy *v1alpha1.NicClusterPolicy, manifestRenderer state.ManifestRenderer,
	allErrs field.ErrorList, fp *field.Path, child string) field.ErrorList {
	if len(resources) == 0 {
		allErrs = append(allErrs, field.Invalid(fp.Child(child).Child("containerResources"),
			resources, "should not be empty if declared"))
	}

	supportedContainerNames, err := state.ParseContainerNames(manifestRenderer, policy, nicClusterPolicyLog)
	if err != nil {
		nicClusterPolicyLog.Error(err, "failed to parse container names")
	}

	for _, reqs := range resources {
		allErrs = validateResources(reqs.Requests, allErrs, fp, child, "Requests")
		allErrs = validateResources(reqs.Limits, allErrs, fp, child, "Limits", reqs.Requests)
		if !slices.Contains(supportedContainerNames, reqs.Name) {
			allErrs = append(
				allErrs, field.Invalid(fp.Child(child).Child("containerResources").Child("name"),
					reqs.Name, fmt.Sprintf("container name %s is unsupported for this state", reqs.Name)))
		}
	}

	return allErrs
}

func validateResources(resources map[v1.ResourceName]apiresource.Quantity, allErrs field.ErrorList, fp *field.Path,
	child, resourceType string, requests ...map[v1.ResourceName]apiresource.Quantity) field.ErrorList {
	for resourceName, quantity := range resources {
		if resourceName == v1.ResourceCPU || resourceName == v1.ResourceMemory {
			if quantity.IsZero() {
				allErrs = append(allErrs, field.Invalid(fp.Child(child).Child("containerResources").
					Child(resourceType).Child(string(resourceName)),
					quantity, fmt.Sprintf("resource %s for %s is zero", resourceType, string(resourceName))))
			}
		}

		if resourceType == "Limits" && len(requests) > 0 && requests[0] != nil {
			if requestQuantity, hasRequest := requests[0][resourceName]; hasRequest {
				if quantity.Cmp(requestQuantity) < 0 {
					allErrs = append(allErrs, field.Invalid(fp.Child(child).Child("containerResources").
						Child("Requests").Child(string(resourceName)), quantity,
						fmt.Sprintf("resource request for %s is greater than the limit", string(resourceName))))
				}
			}
		}
	}

	return allErrs
}

// isValidOFEDVersion is a custom function to validate OFED version
func isValidOFEDVersion(version string) bool {
	versionPattern := `^(\d+\.\d+-\d+(\.\d+)*)$`
	versionRegex := regexp.MustCompile(versionPattern)
	return versionRegex.MatchString(version)
}

func isValidSriovNetworkDevicePluginResourceName(resourceName string) bool {
	resourceNameRegex := regexp.MustCompile(sriovResourceNameRegex)
	return resourceNameRegex.MatchString(resourceName)
}

func isValidRdmaSharedDevicePluginResourceName(resourceName string) bool {
	resourceNameRegex := regexp.MustCompile(rdmaResourceNameRegex)
	return resourceNameRegex.MatchString(resourceName)
}

func isValidFQDN(input string) bool {
	regex := regexp.MustCompile(fqdnRegex)
	return regex.MatchString(input)
}

// +kubebuilder:object:generate=false
type schemaValidator struct {
	schemas map[string]*gojsonschema.Schema
}

func (sv *schemaValidator) GetSchema(schemaName string) (*gojsonschema.Schema, error) {
	s, ok := sv.schemas[schemaName]
	if !ok {
		return nil, fmt.Errorf("validation schema not found: %s", schemaName)
	}
	return s, nil
}

func InitSchemaValidator(schemaPath string) {
	sv := &schemaValidator{
		schemas: make(map[string]*gojsonschema.Schema),
	}
	files, err := os.ReadDir(schemaPath)
	if err != nil {
		nicClusterPolicyLog.Error(err, "fail to read validation schema files")
		panic(err)
	}
	for _, f := range files {
		s, err := gojsonschema.NewSchema(gojsonschema.NewReferenceLoader(fmt.Sprintf("file://%s/%s", schemaPath, f.Name())))
		if err != nil {
			nicClusterPolicyLog.Error(err, "fail to load validation schema")
			panic(err)
		}
		sv.schemas[strings.TrimSuffix(f.Name(), ".json")] = s
	}
	schemaValidators = sv
}

// DisableValidations will disable all CRs admission validations
func DisableValidations() {
	skipValidations = true
}
