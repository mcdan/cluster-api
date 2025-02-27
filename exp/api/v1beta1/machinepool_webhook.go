/*
Copyright 2021 The Kubernetes Authors.

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

package v1beta1

import (
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/feature"
	"sigs.k8s.io/cluster-api/util/version"
)

func (m *MachinePool) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(m).
		Complete()
}

// +kubebuilder:webhook:verbs=create;update,path=/validate-cluster-x-k8s-io-v1beta1-machinepool,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,groups=cluster.x-k8s.io,resources=machinepools,versions=v1beta1,name=validation.machinepool.cluster.x-k8s.io,sideEffects=None,admissionReviewVersions=v1;v1beta1
// +kubebuilder:webhook:verbs=create;update,path=/mutate-cluster-x-k8s-io-v1beta1-machinepool,mutating=true,failurePolicy=fail,matchPolicy=Equivalent,groups=cluster.x-k8s.io,resources=machinepools,versions=v1beta1,name=default.machinepool.cluster.x-k8s.io,sideEffects=None,admissionReviewVersions=v1;v1beta1

var _ webhook.Defaulter = &MachinePool{}
var _ webhook.Validator = &MachinePool{}

// Default implements webhook.Defaulter so a webhook will be registered for the type.
func (m *MachinePool) Default() {
	if m.Labels == nil {
		m.Labels = make(map[string]string)
	}
	m.Labels[clusterv1.ClusterNameLabel] = m.Spec.ClusterName

	if m.Spec.Replicas == nil {
		m.Spec.Replicas = pointer.Int32(1)
	}

	if m.Spec.MinReadySeconds == nil {
		m.Spec.MinReadySeconds = pointer.Int32(0)
	}

	if m.Spec.Template.Spec.Bootstrap.ConfigRef != nil && m.Spec.Template.Spec.Bootstrap.ConfigRef.Namespace == "" {
		m.Spec.Template.Spec.Bootstrap.ConfigRef.Namespace = m.Namespace
	}

	if m.Spec.Template.Spec.InfrastructureRef.Namespace == "" {
		m.Spec.Template.Spec.InfrastructureRef.Namespace = m.Namespace
	}

	// tolerate version strings without a "v" prefix: prepend it if it's not there.
	if m.Spec.Template.Spec.Version != nil && !strings.HasPrefix(*m.Spec.Template.Spec.Version, "v") {
		normalizedVersion := "v" + *m.Spec.Template.Spec.Version
		m.Spec.Template.Spec.Version = &normalizedVersion
	}
}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type.
func (m *MachinePool) ValidateCreate() (admission.Warnings, error) {
	return nil, m.validate(nil)
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type.
func (m *MachinePool) ValidateUpdate(old runtime.Object) (admission.Warnings, error) {
	oldMP, ok := old.(*MachinePool)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a MachinePool but got a %T", old))
	}
	return nil, m.validate(oldMP)
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type.
func (m *MachinePool) ValidateDelete() (admission.Warnings, error) {
	return nil, m.validate(nil)
}

func (m *MachinePool) validate(old *MachinePool) error {
	// NOTE: MachinePool is behind MachinePool feature gate flag; the web hook
	// must prevent creating new objects when the feature flag is disabled.
	specPath := field.NewPath("spec")
	if !feature.Gates.Enabled(feature.MachinePool) {
		return field.Forbidden(
			specPath,
			"can be set only if the MachinePool feature flag is enabled",
		)
	}
	var allErrs field.ErrorList
	if m.Spec.Template.Spec.Bootstrap.ConfigRef == nil && m.Spec.Template.Spec.Bootstrap.DataSecretName == nil {
		allErrs = append(
			allErrs,
			field.Required(
				specPath.Child("template", "spec", "bootstrap", "data"),
				"expected either spec.bootstrap.dataSecretName or spec.bootstrap.configRef to be populated",
			),
		)
	}

	if m.Spec.Template.Spec.Bootstrap.ConfigRef != nil && m.Spec.Template.Spec.Bootstrap.ConfigRef.Namespace != m.Namespace {
		allErrs = append(
			allErrs,
			field.Invalid(
				specPath.Child("template", "spec", "bootstrap", "configRef", "namespace"),
				m.Spec.Template.Spec.Bootstrap.ConfigRef.Namespace,
				"must match metadata.namespace",
			),
		)
	}

	if m.Spec.Template.Spec.InfrastructureRef.Namespace != m.Namespace {
		allErrs = append(
			allErrs,
			field.Invalid(
				specPath.Child("infrastructureRef", "namespace"),
				m.Spec.Template.Spec.InfrastructureRef.Namespace,
				"must match metadata.namespace",
			),
		)
	}

	if old != nil && old.Spec.ClusterName != m.Spec.ClusterName {
		allErrs = append(
			allErrs,
			field.Forbidden(
				specPath.Child("clusterName"),
				"field is immutable"),
		)
	}

	if m.Spec.Template.Spec.Version != nil {
		if !version.KubeSemver.MatchString(*m.Spec.Template.Spec.Version) {
			allErrs = append(allErrs, field.Invalid(specPath.Child("template", "spec", "version"), *m.Spec.Template.Spec.Version, "must be a valid semantic version"))
		}
	}

	// Validate the metadata of the MachinePool template.
	allErrs = append(allErrs, m.Spec.Template.ObjectMeta.Validate(specPath.Child("template", "metadata"))...)

	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(GroupVersion.WithKind("MachinePool").GroupKind(), m.Name, allErrs)
}
