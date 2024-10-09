/*
Copyright 2021 The KServe Authors.

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

package service

import (
	"context"
	"fmt"
	"strconv"

	"github.com/kserve/kserve/pkg/apis/serving/v1beta1"
	"github.com/kserve/kserve/pkg/constants"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var log = logf.Log.WithName("ServiceReconciler")

// ServiceReconciler is the struct of Raw K8S Object
type ServiceReconciler struct {
	client       client.Client
	scheme       *runtime.Scheme
	Service      *corev1.Service
	componentExt *v1beta1.ComponentExtensionSpec
}

func NewServiceReconciler(client client.Client,
	scheme *runtime.Scheme,
	componentMeta metav1.ObjectMeta,
	componentExt *v1beta1.ComponentExtensionSpec,
	podSpec *corev1.PodSpec) *ServiceReconciler {
	return &ServiceReconciler{
		client:       client,
		scheme:       scheme,
		Service:      createService(componentMeta, componentExt, podSpec),
		componentExt: componentExt,
	}
}

func createService(componentMeta metav1.ObjectMeta, componentExt *v1beta1.ComponentExtensionSpec,
	podSpec *corev1.PodSpec) *corev1.Service {
	var servicePorts []corev1.ServicePort
	if len(podSpec.Containers) != 0 {
		container := podSpec.Containers[0]
		for _, c := range podSpec.Containers {
			if c.Name == constants.TransformerContainerName {
				container = c
				break
			}
		}
		if len(container.Ports) > 0 {
			var servicePort corev1.ServicePort
			servicePort = corev1.ServicePort{
				Name: container.Ports[0].Name,
				Port: constants.CommonDefaultHttpPort,
				TargetPort: intstr.IntOrString{
					Type:   intstr.Int,
					IntVal: container.Ports[0].ContainerPort,
				},
				Protocol: container.Ports[0].Protocol,
			}
			if servicePort.Name == "" {
				servicePort.Name = "http"
			}
			servicePorts = append(servicePorts, servicePort)

			for i := 1; i < len(container.Ports); i++ {
				port := container.Ports[i]
				if port.Protocol == "" {
					port.Protocol = corev1.ProtocolTCP
				}
				servicePort = corev1.ServicePort{
					Name: port.Name,
					Port: port.ContainerPort,
					TargetPort: intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: port.ContainerPort,
					},
					Protocol: port.Protocol,
				}
				servicePorts = append(servicePorts, servicePort)
			}
		} else {
			port, _ := strconv.Atoi(constants.InferenceServiceDefaultHttpPort)
			servicePorts = append(servicePorts, corev1.ServicePort{
				Name: componentMeta.Name,
				Port: constants.CommonDefaultHttpPort,
				TargetPort: intstr.IntOrString{
					Type:   intstr.Int,
					IntVal: int32(port), // #nosec G109
				},
				Protocol: corev1.ProtocolTCP,
			})
		}
	}
	if componentExt != nil && componentExt.Batcher != nil {
		servicePorts[0].TargetPort = intstr.IntOrString{
			Type:   intstr.Int,
			IntVal: constants.InferenceServiceDefaultAgentPort,
		}
	}
	if componentExt != nil && componentExt.Logger != nil {
		servicePorts[0].TargetPort = intstr.IntOrString{
			Type:   intstr.Int,
			IntVal: constants.InferenceServiceDefaultAgentPort,
		}
	}

	service := &corev1.Service{
		ObjectMeta: componentMeta,
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app": constants.GetRawServiceLabel(componentMeta.Name),
			},
			Ports: servicePorts,
			// TODO - add a control flag
			// Need to add a control flag to properly set it, enable/disable this behavior.
			// Follow up issue to align with upstream: https://issues.redhat.com/browse/RHOAIENG-5077
			ClusterIP: corev1.ClusterIPNone,
		},
	}
	if val, ok := componentMeta.Labels[constants.ODHKserveRawAuth]; ok && val == "true" {
		if service.ObjectMeta.Annotations == nil {
			service.ObjectMeta.Annotations = make(map[string]string)
		}
		service.ObjectMeta.Annotations["service.beta.openshift.io/serving-cert-secret-name"] = componentMeta.Name
		httpsPort := corev1.ServicePort{
			Name: "https",
			Port: constants.OauthProxyPort,
			TargetPort: intstr.IntOrString{
				Type:   intstr.String,
				StrVal: "https",
			},
			Protocol: corev1.ProtocolTCP,
		}
		service.Spec.Ports = append(service.Spec.Ports, httpsPort)
	}

	for index, port := range service.Spec.Ports {
		fmt.Println(index, port.Name, port.Port, port.TargetPort)
	}
	return service
}

// checkServiceExist checks if the service exists?
func (r *ServiceReconciler) checkServiceExist(client client.Client) (constants.CheckResultType, *corev1.Service, error) {
	// get service
	existingService := &corev1.Service{}
	err := client.Get(context.TODO(), types.NamespacedName{
		Namespace: r.Service.Namespace,
		Name:      r.Service.Name,
	}, existingService)
	if err != nil {
		if apierr.IsNotFound(err) {
			return constants.CheckResultCreate, nil, nil
		}
		return constants.CheckResultUnknown, nil, err
	}

	// existed, check equivalent
	if semanticServiceEquals(r.Service, existingService) {
		return constants.CheckResultExisted, existingService, nil
	}
	return constants.CheckResultUpdate, existingService, nil
}

func semanticServiceEquals(desired, existing *corev1.Service) bool {
	return equality.Semantic.DeepEqual(desired.Spec.Ports, existing.Spec.Ports) &&
		equality.Semantic.DeepEqual(desired.Spec.Selector, existing.Spec.Selector)
}

// Reconcile ...
func (r *ServiceReconciler) Reconcile() (*corev1.Service, error) {
	// reconcile Service
	checkResult, existingService, err := r.checkServiceExist(r.client)
	log.Info("service reconcile", "checkResult", checkResult, "err", err)
	if err != nil {
		return nil, err
	}

	var opErr error
	switch checkResult {
	case constants.CheckResultCreate:
		opErr = r.client.Create(context.TODO(), r.Service)
	case constants.CheckResultUpdate:
		opErr = r.client.Update(context.TODO(), r.Service)
	default:
		return existingService, nil
	}

	if opErr != nil {
		return nil, opErr
	}

	return r.Service, nil
}
