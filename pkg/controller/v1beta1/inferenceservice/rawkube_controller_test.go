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

package inferenceservice

import (
	"context"
	"fmt"
	"time"

	"github.com/kserve/kserve/pkg/apis/serving/v1alpha1"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/kserve/kserve/pkg/apis/serving/v1beta1"
	"github.com/kserve/kserve/pkg/constants"
	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	v1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("v1beta1 inference service controller", func() {
	// Define utility constants for object names and testing timeouts/durations and intervals.
	const (
		timeout  = time.Second * 10
		interval = time.Millisecond * 250
		domain   = "example.com"
	)
	var (
		defaultResource = v1.ResourceRequirements{
			Limits: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("1"),
				v1.ResourceMemory: resource.MustParse("2Gi"),
			},
			Requests: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("1"),
				v1.ResourceMemory: resource.MustParse("2Gi"),
			},
		}
	)

	Context("When creating inference service with raw kube predictor", func() {
		configs := map[string]string{
			"explainers": `{
				"alibi": {
					"image": "kserve/alibi-explainer",
					"defaultImageVersion": "latest"
				}
			}`,
			"ingress": `{
				"ingressGateway": "knative-serving/knative-ingress-gateway",
				"ingressService": "test-destination",
				"localGateway": "knative-serving/knative-local-gateway",
				"localGatewayService": "knative-local-gateway.istio-system.svc.cluster.local"
			}`,
			"storageInitializer": `{
				"image" : "kserve/storage-initializer:latest",
				"memoryRequest": "100Mi",
				"memoryLimit": "1Gi",
				"cpuRequest": "100m",
				"cpuLimit": "1",
				"CaBundleConfigMapName": "",
				"caBundleVolumeMountPath": "/etc/ssl/custom-certs",
				"enableDirectPvcVolumeMount": false
			}`,
		}

		It("Should have ingress/service/deployment/hpa created", func() {
			By("By creating a new InferenceService")
			// Create configmap
			var configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      constants.InferenceServiceConfigMapName,
					Namespace: constants.KServeNamespace,
				},
				Data: configs,
			}
			Expect(k8sClient.Create(context.TODO(), configMap)).NotTo(HaveOccurred())
			defer k8sClient.Delete(context.TODO(), configMap)
			// Create ServingRuntime
			servingRuntime := &v1alpha1.ServingRuntime{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tf-serving-raw",
					Namespace: "default",
				},
				Spec: v1alpha1.ServingRuntimeSpec{
					SupportedModelFormats: []v1alpha1.SupportedModelFormat{
						{
							Name:       "tensorflow",
							Version:    proto.String("1"),
							AutoSelect: proto.Bool(true),
						},
					},
					ServingRuntimePodSpec: v1alpha1.ServingRuntimePodSpec{
						Containers: []v1.Container{
							{
								Name:    "kserve-container",
								Image:   "tensorflow/serving:1.14.0",
								Command: []string{"/usr/bin/tensorflow_model_server"},
								Args: []string{
									"--port=9000",
									"--rest_api_port=8080",
									"--model_base_path=/mnt/models",
									"--rest_api_timeout_in_ms=60000",
								},
								Resources: defaultResource,
							},
						},
					},
					Disabled: proto.Bool(false),
				},
			}
			k8sClient.Create(context.TODO(), servingRuntime)
			defer k8sClient.Delete(context.TODO(), servingRuntime)
			serviceName := "raw-foo"
			var expectedRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: serviceName, Namespace: "default"}}
			var serviceKey = expectedRequest.NamespacedName
			var storageUri = "s3://test/mnist/export"
			ctx := context.Background()
			isvc := &v1beta1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceKey.Name,
					Namespace: serviceKey.Namespace,
					Annotations: map[string]string{
						"serving.kserve.io/deploymentMode":              "RawDeployment",
						"serving.kserve.io/autoscalerClass":             "hpa",
						"serving.kserve.io/metrics":                     "cpu",
						"serving.kserve.io/targetUtilizationPercentage": "75",
					},
				},
				Spec: v1beta1.InferenceServiceSpec{
					Predictor: v1beta1.PredictorSpec{
						ComponentExtensionSpec: v1beta1.ComponentExtensionSpec{
							MinReplicas: v1beta1.GetIntReference(1),
							MaxReplicas: 3,
						},
						Tensorflow: &v1beta1.TFServingSpec{
							PredictorExtensionSpec: v1beta1.PredictorExtensionSpec{
								StorageURI:     &storageUri,
								RuntimeVersion: proto.String("1.14.0"),
								Container: v1.Container{
									Name:      constants.InferenceServiceContainerName,
									Resources: defaultResource,
								},
							},
						},
					},
				},
			}
			isvc.DefaultInferenceService(nil, nil)
			Expect(k8sClient.Create(ctx, isvc)).Should(Succeed())

			inferenceService := &v1beta1.InferenceService{}

			Eventually(func() bool {
				err := k8sClient.Get(ctx, serviceKey, inferenceService)
				if err != nil {
					return false
				}
				return true
			}, timeout, interval).Should(BeTrue())

			actualDeployment := &appsv1.Deployment{}
			predictorDeploymentKey := types.NamespacedName{Name: constants.PredictorServiceName(serviceKey.Name),
				Namespace: serviceKey.Namespace}
			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorDeploymentKey, actualDeployment) }, timeout).
				Should(Succeed())
			var replicas int32 = 1
			var revisionHistory int32 = 10
			var progressDeadlineSeconds int32 = 600
			var gracePeriod int64 = 30
			expectedDeployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      predictorDeploymentKey.Name,
					Namespace: predictorDeploymentKey.Namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "isvc." + predictorDeploymentKey.Name,
						},
					},
					Template: v1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Name:      predictorDeploymentKey.Name,
							Namespace: "default",
							Labels: map[string]string{
								"app":                                 "isvc." + predictorDeploymentKey.Name,
								constants.KServiceComponentLabel:      constants.Predictor.String(),
								constants.InferenceServicePodLabelKey: serviceName,
							},
							Annotations: map[string]string{
								constants.StorageInitializerSourceUriInternalAnnotationKey: *isvc.Spec.Predictor.Model.StorageURI,
								"serving.kserve.io/deploymentMode":                         "RawDeployment",
								"serving.kserve.io/autoscalerClass":                        "hpa",
								"serving.kserve.io/metrics":                                "cpu",
								"serving.kserve.io/targetUtilizationPercentage":            "75",
							},
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Image: "tensorflow/serving:" +
										*isvc.Spec.Predictor.Model.RuntimeVersion,
									Name:    constants.InferenceServiceContainerName,
									Command: []string{v1beta1.TensorflowEntrypointCommand},
									Args: []string{
										"--port=" + v1beta1.TensorflowServingGRPCPort,
										"--rest_api_port=" + v1beta1.TensorflowServingRestPort,
										"--model_base_path=" + constants.DefaultModelLocalMountPath,
										"--rest_api_timeout_in_ms=60000",
									},
									Resources: defaultResource,
									ReadinessProbe: &v1.Probe{
										ProbeHandler: v1.ProbeHandler{
											TCPSocket: &v1.TCPSocketAction{
												Port: intstr.IntOrString{
													IntVal: 8080,
												},
											},
										},
										InitialDelaySeconds: 0,
										TimeoutSeconds:      1,
										PeriodSeconds:       10,
										SuccessThreshold:    1,
										FailureThreshold:    3,
									},
									TerminationMessagePath:   "/dev/termination-log",
									TerminationMessagePolicy: "File",
									ImagePullPolicy:          "IfNotPresent",
								},
							},
							SchedulerName:                 "default-scheduler",
							RestartPolicy:                 "Always",
							TerminationGracePeriodSeconds: &gracePeriod,
							DNSPolicy:                     "ClusterFirst",
							SecurityContext: &v1.PodSecurityContext{
								SELinuxOptions:      nil,
								WindowsOptions:      nil,
								RunAsUser:           nil,
								RunAsGroup:          nil,
								RunAsNonRoot:        nil,
								SupplementalGroups:  nil,
								FSGroup:             nil,
								Sysctls:             nil,
								FSGroupChangePolicy: nil,
								SeccompProfile:      nil,
							},
							ServiceAccountName:       constants.KserveServiceAccountName,
							DeprecatedServiceAccount: constants.KserveServiceAccountName,
						},
					},
					Strategy: appsv1.DeploymentStrategy{
						Type: "RollingUpdate",
						RollingUpdate: &appsv1.RollingUpdateDeployment{
							MaxUnavailable: &intstr.IntOrString{Type: 1, IntVal: 0, StrVal: "25%"},
							MaxSurge:       &intstr.IntOrString{Type: 1, IntVal: 0, StrVal: "25%"},
						},
					},
					RevisionHistoryLimit:    &revisionHistory,
					ProgressDeadlineSeconds: &progressDeadlineSeconds,
				},
			}
			Expect(actualDeployment.Spec).To(gomega.Equal(expectedDeployment.Spec))

			//check service
			actualService := &v1.Service{}
			predictorServiceKey := types.NamespacedName{Name: constants.PredictorServiceName(serviceKey.Name),
				Namespace: serviceKey.Namespace}
			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorServiceKey, actualService) }, timeout).
				Should(Succeed())

			expectedService := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      predictorServiceKey.Name,
					Namespace: predictorServiceKey.Namespace,
				},
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{
						{
							Name:       constants.PredictorServiceName(serviceName),
							Protocol:   "TCP",
							Port:       80,
							TargetPort: intstr.IntOrString{Type: 0, IntVal: 8080, StrVal: ""},
						},
					},
					Type:            "ClusterIP",
					SessionAffinity: "None",
					Selector: map[string]string{
						"app": fmt.Sprintf("isvc.%s", constants.PredictorServiceName(serviceName)),
					},
				},
			}
			actualService.Spec.ClusterIP = ""
			actualService.Spec.ClusterIPs = nil
			actualService.Spec.IPFamilies = nil
			actualService.Spec.IPFamilyPolicy = nil
			actualService.Spec.InternalTrafficPolicy = nil
			Expect(actualService.Spec).To(gomega.Equal(expectedService.Spec))

			//check isvc status
			updatedDeployment := actualDeployment.DeepCopy()
			updatedDeployment.Status.Conditions = []appsv1.DeploymentCondition{
				{
					Type:   appsv1.DeploymentAvailable,
					Status: v1.ConditionTrue,
				},
			}
			Expect(k8sClient.Status().Update(context.TODO(), updatedDeployment)).NotTo(gomega.HaveOccurred())

			//check ingress
			pathType := netv1.PathTypePrefix
			actualIngress := &netv1.Ingress{}
			predictorIngressKey := types.NamespacedName{Name: serviceKey.Name,
				Namespace: serviceKey.Namespace}
			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorIngressKey, actualIngress) }, timeout).
				Should(Succeed())
			expectedIngress := netv1.Ingress{
				Spec: netv1.IngressSpec{
					Rules: []netv1.IngressRule{
						{
							Host: "raw-foo-default.example.com",
							IngressRuleValue: netv1.IngressRuleValue{
								HTTP: &netv1.HTTPIngressRuleValue{
									Paths: []netv1.HTTPIngressPath{
										{
											Path:     "/",
											PathType: &pathType,
											Backend: netv1.IngressBackend{
												Service: &netv1.IngressServiceBackend{
													Name: "raw-foo-predictor",
													Port: netv1.ServiceBackendPort{
														Number: 80,
													},
												},
											},
										},
									},
								},
							},
						},
						{
							Host: "raw-foo-predictor-default.example.com",
							IngressRuleValue: netv1.IngressRuleValue{
								HTTP: &netv1.HTTPIngressRuleValue{
									Paths: []netv1.HTTPIngressPath{
										{
											Path:     "/",
											PathType: &pathType,
											Backend: netv1.IngressBackend{
												Service: &netv1.IngressServiceBackend{
													Name: "raw-foo-predictor",
													Port: netv1.ServiceBackendPort{
														Number: 80,
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			}
			Expect(actualIngress.Spec).To(gomega.Equal(expectedIngress.Spec))
			// verify if InferenceService status is updated
			expectedIsvcStatus := v1beta1.InferenceServiceStatus{
				Status: duckv1.Status{
					Conditions: duckv1.Conditions{
						{
							Type:   v1beta1.IngressReady,
							Status: "True",
						},
						{
							Type:   v1beta1.PredictorReady,
							Status: "True",
						},
						{
							Type:   apis.ConditionReady,
							Status: "True",
						},
					},
				},
				URL: &apis.URL{
					Scheme: "http",
					Host:   "raw-foo-default.example.com",
				},
				Address: &duckv1.Addressable{
					URL: &apis.URL{
						Scheme: "http",
						Host:   fmt.Sprintf("%s-predictor.%s.svc.cluster.local", serviceKey.Name, serviceKey.Namespace),
					},
				},
				Components: map[v1beta1.ComponentType]v1beta1.ComponentStatusSpec{
					v1beta1.PredictorComponent: {
						LatestCreatedRevision: "",
						URL: &apis.URL{
							Scheme: "http",
							Host:   "raw-foo-predictor-default.example.com",
						},
					},
				},
				ModelStatus: v1beta1.ModelStatus{
					TransitionStatus:    "InProgress",
					ModelRevisionStates: &v1beta1.ModelRevisionStates{TargetModelState: "Pending"},
				},
			}
			Eventually(func() string {
				isvc := &v1beta1.InferenceService{}
				if err := k8sClient.Get(context.TODO(), serviceKey, isvc); err != nil {
					return err.Error()
				}
				return cmp.Diff(&expectedIsvcStatus, &isvc.Status, cmpopts.IgnoreTypes(apis.VolatileTime{}))
			}, timeout).Should(gomega.BeEmpty())

			//check HPA
			var minReplicas int32 = 1
			var maxReplicas int32 = 3
			var cpuUtilization int32 = 75
			var stabilizationWindowSeconds int32 = 0
			selectPolicy := autoscalingv2.MaxChangePolicySelect
			actualHPA := &autoscalingv2.HorizontalPodAutoscaler{}
			predictorHPAKey := types.NamespacedName{Name: constants.PredictorServiceName(serviceKey.Name),
				Namespace: serviceKey.Namespace}
			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorHPAKey, actualHPA) }, timeout).
				Should(Succeed())
			expectedHPA := &autoscalingv2.HorizontalPodAutoscaler{
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       constants.PredictorServiceName(serviceKey.Name),
					},
					MinReplicas: &minReplicas,
					MaxReplicas: maxReplicas,
					Metrics: []autoscalingv2.MetricSpec{
						{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								Name: v1.ResourceCPU,
								Target: autoscalingv2.MetricTarget{
									Type:               "Utilization",
									AverageUtilization: &cpuUtilization,
								},
							},
						},
					},
					Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
						ScaleUp: &autoscalingv2.HPAScalingRules{
							StabilizationWindowSeconds: &stabilizationWindowSeconds,
							SelectPolicy:               &selectPolicy,
							Policies: []autoscalingv2.HPAScalingPolicy{
								{
									Type:          "Pods",
									Value:         4,
									PeriodSeconds: 15,
								},
								{
									Type:          "Percent",
									Value:         100,
									PeriodSeconds: 15,
								},
							},
						},
						ScaleDown: &autoscalingv2.HPAScalingRules{
							StabilizationWindowSeconds: nil,
							SelectPolicy:               &selectPolicy,
							Policies: []autoscalingv2.HPAScalingPolicy{
								{
									Type:          "Percent",
									Value:         100,
									PeriodSeconds: 15,
								},
							},
						},
					},
				},
			}
			Expect(actualHPA.Spec).To(gomega.Equal(expectedHPA.Spec))
		})
		It("Should have ingress/service/deployment/hpa created with DeploymentStrategy", func() {
			By("By creating a new InferenceService with DeploymentStrategy in PredictorSpec")
			// Create configmap
			var configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      constants.InferenceServiceConfigMapName,
					Namespace: constants.KServeNamespace,
				},
				Data: configs,
			}
			Expect(k8sClient.Create(context.TODO(), configMap)).NotTo(HaveOccurred())
			defer k8sClient.Delete(context.TODO(), configMap)
			// Create ServingRuntime
			servingRuntime := &v1alpha1.ServingRuntime{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tf-serving-raw",
					Namespace: "default",
				},
				Spec: v1alpha1.ServingRuntimeSpec{
					SupportedModelFormats: []v1alpha1.SupportedModelFormat{
						{
							Name:       "tensorflow",
							Version:    proto.String("1"),
							AutoSelect: proto.Bool(true),
						},
					},
					ServingRuntimePodSpec: v1alpha1.ServingRuntimePodSpec{
						Containers: []v1.Container{
							{
								Name:    "kserve-container",
								Image:   "tensorflow/serving:1.14.0",
								Command: []string{"/usr/bin/tensorflow_model_server"},
								Args: []string{
									"--port=9000",
									"--rest_api_port=8080",
									"--model_base_path=/mnt/models",
									"--rest_api_timeout_in_ms=60000",
								},
								Resources: defaultResource,
							},
						},
					},
					Disabled: proto.Bool(false),
				},
			}
			k8sClient.Create(context.TODO(), servingRuntime)
			defer k8sClient.Delete(context.TODO(), servingRuntime)
			serviceName := "raw-foo-customized"
			var expectedRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: serviceName, Namespace: "default"}}
			var serviceKey = expectedRequest.NamespacedName
			var storageUri = "s3://test/mnist/export"
			predictorDeploymentKey := types.NamespacedName{Name: constants.PredictorServiceName(serviceKey.Name),
				Namespace: serviceKey.Namespace}
			var replicas int32 = 1
			var revisionHistory int32 = 10
			var progressDeadlineSeconds int32 = 600
			var gracePeriod int64 = 30
			ctx := context.Background()
			isvc := &v1beta1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceKey.Name,
					Namespace: serviceKey.Namespace,
					Annotations: map[string]string{
						"serving.kserve.io/deploymentMode":              "RawDeployment",
						"serving.kserve.io/autoscalerClass":             "hpa",
						"serving.kserve.io/metrics":                     "cpu",
						"serving.kserve.io/targetUtilizationPercentage": "75",
					},
				},
				Spec: v1beta1.InferenceServiceSpec{
					Predictor: v1beta1.PredictorSpec{
						ComponentExtensionSpec: v1beta1.ComponentExtensionSpec{
							MinReplicas: v1beta1.GetIntReference(1),
							MaxReplicas: 3,
							DeploymentStrategy: &appsv1.DeploymentStrategy{
								Type: appsv1.RecreateDeploymentStrategyType,
							}},
						Tensorflow: &v1beta1.TFServingSpec{
							PredictorExtensionSpec: v1beta1.PredictorExtensionSpec{
								StorageURI:     &storageUri,
								RuntimeVersion: proto.String("1.14.0"),
								Container: v1.Container{
									Name:      constants.InferenceServiceContainerName,
									Resources: defaultResource,
								},
							},
						},
					},
				},
			}
			isvc.DefaultInferenceService(nil, nil)
			Expect(k8sClient.Create(ctx, isvc)).Should(Succeed())

			inferenceService := &v1beta1.InferenceService{}

			Eventually(func() bool {
				err := k8sClient.Get(ctx, serviceKey, inferenceService)
				if err != nil {
					return false
				}
				return true
			}, timeout, interval).Should(BeTrue())

			actualDeployment := &appsv1.Deployment{}

			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorDeploymentKey, actualDeployment) }, timeout).
				Should(Succeed())

			expectedDeployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      predictorDeploymentKey.Name,
					Namespace: predictorDeploymentKey.Namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "isvc." + predictorDeploymentKey.Name,
						},
					},
					Template: v1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Name:      predictorDeploymentKey.Name,
							Namespace: "default",
							Labels: map[string]string{
								"app":                                 "isvc." + predictorDeploymentKey.Name,
								constants.KServiceComponentLabel:      constants.Predictor.String(),
								constants.InferenceServicePodLabelKey: serviceName,
							},
							Annotations: map[string]string{
								constants.StorageInitializerSourceUriInternalAnnotationKey: *isvc.Spec.Predictor.Model.StorageURI,
								"serving.kserve.io/deploymentMode":                         "RawDeployment",
								"serving.kserve.io/autoscalerClass":                        "hpa",
								"serving.kserve.io/metrics":                                "cpu",
								"serving.kserve.io/targetUtilizationPercentage":            "75",
							},
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Image: "tensorflow/serving:" +
										*isvc.Spec.Predictor.Model.RuntimeVersion,
									Name:    constants.InferenceServiceContainerName,
									Command: []string{v1beta1.TensorflowEntrypointCommand},
									Args: []string{
										"--port=" + v1beta1.TensorflowServingGRPCPort,
										"--rest_api_port=" + v1beta1.TensorflowServingRestPort,
										"--model_base_path=" + constants.DefaultModelLocalMountPath,
										"--rest_api_timeout_in_ms=60000",
									},
									Resources: defaultResource,
									ReadinessProbe: &v1.Probe{
										ProbeHandler: v1.ProbeHandler{
											TCPSocket: &v1.TCPSocketAction{
												Port: intstr.IntOrString{
													IntVal: 8080,
												},
											},
										},
										InitialDelaySeconds: 0,
										TimeoutSeconds:      1,
										PeriodSeconds:       10,
										SuccessThreshold:    1,
										FailureThreshold:    3,
									},
									TerminationMessagePath:   "/dev/termination-log",
									TerminationMessagePolicy: "File",
									ImagePullPolicy:          "IfNotPresent",
								},
							},
							SchedulerName:                 "default-scheduler",
							RestartPolicy:                 "Always",
							TerminationGracePeriodSeconds: &gracePeriod,
							DNSPolicy:                     "ClusterFirst",
							SecurityContext: &v1.PodSecurityContext{
								SELinuxOptions:      nil,
								WindowsOptions:      nil,
								RunAsUser:           nil,
								RunAsGroup:          nil,
								RunAsNonRoot:        nil,
								SupplementalGroups:  nil,
								FSGroup:             nil,
								Sysctls:             nil,
								FSGroupChangePolicy: nil,
								SeccompProfile:      nil,
							},
							ServiceAccountName:       constants.KserveServiceAccountName,
							DeprecatedServiceAccount: constants.KserveServiceAccountName,
						},
					},
					// This is now customized and different from defaults set via `setDefaultDeploymentSpec`.
					Strategy: appsv1.DeploymentStrategy{
						Type: appsv1.RecreateDeploymentStrategyType,
					},
					RevisionHistoryLimit:    &revisionHistory,
					ProgressDeadlineSeconds: &progressDeadlineSeconds,
				},
			}
			Expect(actualDeployment.Spec).To(gomega.Equal(expectedDeployment.Spec))

			//check service
			actualService := &v1.Service{}
			predictorServiceKey := types.NamespacedName{Name: constants.PredictorServiceName(serviceKey.Name),
				Namespace: serviceKey.Namespace}
			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorServiceKey, actualService) }, timeout).
				Should(Succeed())

			expectedService := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      predictorServiceKey.Name,
					Namespace: predictorServiceKey.Namespace,
				},
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{
						{
							Name:       constants.PredictorServiceName(serviceName),
							Protocol:   "TCP",
							Port:       80,
							TargetPort: intstr.IntOrString{Type: 0, IntVal: 8080, StrVal: ""},
						},
					},
					Type:            "ClusterIP",
					SessionAffinity: "None",
					Selector: map[string]string{
						"app": fmt.Sprintf("isvc.%s", constants.PredictorServiceName(serviceName)),
					},
				},
			}
			actualService.Spec.ClusterIP = ""
			actualService.Spec.ClusterIPs = nil
			actualService.Spec.IPFamilies = nil
			actualService.Spec.IPFamilyPolicy = nil
			actualService.Spec.InternalTrafficPolicy = nil
			Expect(actualService.Spec).To(gomega.Equal(expectedService.Spec))

			//check isvc status
			updatedDeployment := actualDeployment.DeepCopy()
			updatedDeployment.Status.Conditions = []appsv1.DeploymentCondition{
				{
					Type:   appsv1.DeploymentAvailable,
					Status: v1.ConditionTrue,
				},
			}
			Expect(k8sClient.Status().Update(context.TODO(), updatedDeployment)).NotTo(gomega.HaveOccurred())

			//check ingress
			pathType := netv1.PathTypePrefix
			actualIngress := &netv1.Ingress{}
			predictorIngressKey := types.NamespacedName{Name: serviceKey.Name,
				Namespace: serviceKey.Namespace}
			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorIngressKey, actualIngress) }, timeout).
				Should(Succeed())
			expectedIngress := netv1.Ingress{
				Spec: netv1.IngressSpec{
					Rules: []netv1.IngressRule{
						{
							Host: "raw-foo-customized-default.example.com",
							IngressRuleValue: netv1.IngressRuleValue{
								HTTP: &netv1.HTTPIngressRuleValue{
									Paths: []netv1.HTTPIngressPath{
										{
											Path:     "/",
											PathType: &pathType,
											Backend: netv1.IngressBackend{
												Service: &netv1.IngressServiceBackend{
													Name: "raw-foo-customized-predictor",
													Port: netv1.ServiceBackendPort{
														Number: 80,
													},
												},
											},
										},
									},
								},
							},
						},
						{
							Host: "raw-foo-customized-predictor-default.example.com",
							IngressRuleValue: netv1.IngressRuleValue{
								HTTP: &netv1.HTTPIngressRuleValue{
									Paths: []netv1.HTTPIngressPath{
										{
											Path:     "/",
											PathType: &pathType,
											Backend: netv1.IngressBackend{
												Service: &netv1.IngressServiceBackend{
													Name: "raw-foo-customized-predictor",
													Port: netv1.ServiceBackendPort{
														Number: 80,
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			}
			Expect(actualIngress.Spec).To(gomega.Equal(expectedIngress.Spec))
			// verify if InferenceService status is updated
			expectedIsvcStatus := v1beta1.InferenceServiceStatus{
				Status: duckv1.Status{
					Conditions: duckv1.Conditions{
						{
							Type:   v1beta1.IngressReady,
							Status: "True",
						},
						{
							Type:   v1beta1.PredictorReady,
							Status: "True",
						},
						{
							Type:   apis.ConditionReady,
							Status: "True",
						},
					},
				},
				URL: &apis.URL{
					Scheme: "http",
					Host:   "raw-foo-customized-default.example.com",
				},
				Address: &duckv1.Addressable{
					URL: &apis.URL{
						Scheme: "http",
						Host:   fmt.Sprintf("%s-predictor.%s.svc.cluster.local", serviceKey.Name, serviceKey.Namespace),
					},
				},
				Components: map[v1beta1.ComponentType]v1beta1.ComponentStatusSpec{
					v1beta1.PredictorComponent: {
						LatestCreatedRevision: "",
						URL: &apis.URL{
							Scheme: "http",
							Host:   "raw-foo-customized-predictor-default.example.com",
						},
					},
				},
				ModelStatus: v1beta1.ModelStatus{
					TransitionStatus:    "InProgress",
					ModelRevisionStates: &v1beta1.ModelRevisionStates{TargetModelState: "Pending"},
				},
			}
			Eventually(func() string {
				isvc := &v1beta1.InferenceService{}
				if err := k8sClient.Get(context.TODO(), serviceKey, isvc); err != nil {
					return err.Error()
				}
				return cmp.Diff(&expectedIsvcStatus, &isvc.Status, cmpopts.IgnoreTypes(apis.VolatileTime{}))
			}, timeout).Should(gomega.BeEmpty())

			//check HPA
			var minReplicas int32 = 1
			var maxReplicas int32 = 3
			var cpuUtilization int32 = 75
			var stabilizationWindowSeconds int32 = 0
			selectPolicy := autoscalingv2.MaxChangePolicySelect
			actualHPA := &autoscalingv2.HorizontalPodAutoscaler{}
			predictorHPAKey := types.NamespacedName{Name: constants.PredictorServiceName(serviceKey.Name),
				Namespace: serviceKey.Namespace}
			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorHPAKey, actualHPA) }, timeout).
				Should(Succeed())
			expectedHPA := &autoscalingv2.HorizontalPodAutoscaler{
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       constants.PredictorServiceName(serviceKey.Name),
					},
					MinReplicas: &minReplicas,
					MaxReplicas: maxReplicas,
					Metrics: []autoscalingv2.MetricSpec{
						{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								Name: v1.ResourceCPU,
								Target: autoscalingv2.MetricTarget{
									Type:               "Utilization",
									AverageUtilization: &cpuUtilization,
								},
							},
						},
					},
					Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
						ScaleUp: &autoscalingv2.HPAScalingRules{
							StabilizationWindowSeconds: &stabilizationWindowSeconds,
							SelectPolicy:               &selectPolicy,
							Policies: []autoscalingv2.HPAScalingPolicy{
								{
									Type:          "Pods",
									Value:         4,
									PeriodSeconds: 15,
								},
								{
									Type:          "Percent",
									Value:         100,
									PeriodSeconds: 15,
								},
							},
						},
						ScaleDown: &autoscalingv2.HPAScalingRules{
							StabilizationWindowSeconds: nil,
							SelectPolicy:               &selectPolicy,
							Policies: []autoscalingv2.HPAScalingPolicy{
								{
									Type:          "Percent",
									Value:         100,
									PeriodSeconds: 15,
								},
							},
						},
					},
				},
			}
			Expect(actualHPA.Spec).To(gomega.Equal(expectedHPA.Spec))
		})
		It("Should have ingress/service/deployment created", func() {
			By("By creating a new InferenceService with AutoscalerClassExternal")
			// Create configmap
			var configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      constants.InferenceServiceConfigMapName,
					Namespace: constants.KServeNamespace,
				},
				Data: configs,
			}
			Expect(k8sClient.Create(context.TODO(), configMap)).NotTo(HaveOccurred())
			defer k8sClient.Delete(context.TODO(), configMap)
			// Create ServingRuntime
			servingRuntime := &v1alpha1.ServingRuntime{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tf-serving-raw",
					Namespace: "default",
				},
				Spec: v1alpha1.ServingRuntimeSpec{
					SupportedModelFormats: []v1alpha1.SupportedModelFormat{
						{
							Name:       "tensorflow",
							Version:    proto.String("1"),
							AutoSelect: proto.Bool(true),
						},
					},
					ServingRuntimePodSpec: v1alpha1.ServingRuntimePodSpec{
						Containers: []v1.Container{
							{
								Name:    "kserve-container",
								Image:   "tensorflow/serving:1.14.0",
								Command: []string{"/usr/bin/tensorflow_model_server"},
								Args: []string{
									"--port=9000",
									"--rest_api_port=8080",
									"--model_base_path=/mnt/models",
									"--rest_api_timeout_in_ms=60000",
								},
								Resources: defaultResource,
							},
						},
					},
					Disabled: proto.Bool(false),
				},
			}
			k8sClient.Create(context.TODO(), servingRuntime)
			defer k8sClient.Delete(context.TODO(), servingRuntime)
			serviceName := "raw-foo-2"
			var expectedRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: serviceName, Namespace: "default"}}
			var serviceKey = expectedRequest.NamespacedName
			var storageUri = "s3://test/mnist/export"
			ctx := context.Background()
			isvc := &v1beta1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceKey.Name,
					Namespace: serviceKey.Namespace,
					Annotations: map[string]string{
						"serving.kserve.io/deploymentMode":  "RawDeployment",
						"serving.kserve.io/autoscalerClass": "external",
					},
				},
				Spec: v1beta1.InferenceServiceSpec{
					Predictor: v1beta1.PredictorSpec{
						Tensorflow: &v1beta1.TFServingSpec{
							PredictorExtensionSpec: v1beta1.PredictorExtensionSpec{
								StorageURI:     &storageUri,
								RuntimeVersion: proto.String("1.14.0"),
								Container: v1.Container{
									Name:      constants.InferenceServiceContainerName,
									Resources: defaultResource,
								},
							},
						},
					},
				},
			}
			isvc.DefaultInferenceService(nil, nil)
			Expect(k8sClient.Create(ctx, isvc)).Should(Succeed())

			inferenceService := &v1beta1.InferenceService{}

			Eventually(func() bool {
				err := k8sClient.Get(ctx, serviceKey, inferenceService)
				if err != nil {
					return false
				}
				return true
			}, timeout, interval).Should(BeTrue())

			actualDeployment := &appsv1.Deployment{}
			predictorDeploymentKey := types.NamespacedName{Name: constants.PredictorServiceName(serviceKey.Name),
				Namespace: serviceKey.Namespace}
			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorDeploymentKey, actualDeployment) }, timeout).
				Should(Succeed())
			var replicas int32 = 1
			var revisionHistory int32 = 10
			var progressDeadlineSeconds int32 = 600
			var gracePeriod int64 = 30
			expectedDeployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      predictorDeploymentKey.Name,
					Namespace: predictorDeploymentKey.Namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "isvc." + predictorDeploymentKey.Name,
						},
					},
					Template: v1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Name:      predictorDeploymentKey.Name,
							Namespace: "default",
							Labels: map[string]string{
								"app":                                 "isvc." + predictorDeploymentKey.Name,
								constants.KServiceComponentLabel:      constants.Predictor.String(),
								constants.InferenceServicePodLabelKey: serviceName,
							},
							Annotations: map[string]string{
								constants.StorageInitializerSourceUriInternalAnnotationKey: *isvc.Spec.Predictor.Model.StorageURI,
								"serving.kserve.io/deploymentMode":                         "RawDeployment",
								"serving.kserve.io/autoscalerClass":                        "external",
							},
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Image: "tensorflow/serving:" +
										*isvc.Spec.Predictor.Model.RuntimeVersion,
									Name:    constants.InferenceServiceContainerName,
									Command: []string{v1beta1.TensorflowEntrypointCommand},
									Args: []string{
										"--port=" + v1beta1.TensorflowServingGRPCPort,
										"--rest_api_port=" + v1beta1.TensorflowServingRestPort,
										"--model_base_path=" + constants.DefaultModelLocalMountPath,
										"--rest_api_timeout_in_ms=60000",
									},
									Resources: defaultResource,
									ReadinessProbe: &v1.Probe{
										ProbeHandler: v1.ProbeHandler{
											TCPSocket: &v1.TCPSocketAction{
												Port: intstr.IntOrString{
													IntVal: 8080,
												},
											},
										},
										InitialDelaySeconds: 0,
										TimeoutSeconds:      1,
										PeriodSeconds:       10,
										SuccessThreshold:    1,
										FailureThreshold:    3,
									},
									TerminationMessagePath:   "/dev/termination-log",
									TerminationMessagePolicy: "File",
									ImagePullPolicy:          "IfNotPresent",
								},
							},
							SchedulerName:                 "default-scheduler",
							RestartPolicy:                 "Always",
							TerminationGracePeriodSeconds: &gracePeriod,
							DNSPolicy:                     "ClusterFirst",
							SecurityContext: &v1.PodSecurityContext{
								SELinuxOptions:      nil,
								WindowsOptions:      nil,
								RunAsUser:           nil,
								RunAsGroup:          nil,
								RunAsNonRoot:        nil,
								SupplementalGroups:  nil,
								FSGroup:             nil,
								Sysctls:             nil,
								FSGroupChangePolicy: nil,
								SeccompProfile:      nil,
							},
							ServiceAccountName:       constants.KserveServiceAccountName,
							DeprecatedServiceAccount: constants.KserveServiceAccountName,
						},
					},
					Strategy: appsv1.DeploymentStrategy{
						Type: "RollingUpdate",
						RollingUpdate: &appsv1.RollingUpdateDeployment{
							MaxUnavailable: &intstr.IntOrString{Type: 1, IntVal: 0, StrVal: "25%"},
							MaxSurge:       &intstr.IntOrString{Type: 1, IntVal: 0, StrVal: "25%"},
						},
					},
					RevisionHistoryLimit:    &revisionHistory,
					ProgressDeadlineSeconds: &progressDeadlineSeconds,
				},
			}
			Expect(actualDeployment.Spec).To(gomega.Equal(expectedDeployment.Spec))

			//check service
			actualService := &v1.Service{}
			predictorServiceKey := types.NamespacedName{Name: constants.PredictorServiceName(serviceKey.Name),
				Namespace: serviceKey.Namespace}
			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorServiceKey, actualService) }, timeout).
				Should(Succeed())

			expectedService := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      predictorServiceKey.Name,
					Namespace: predictorServiceKey.Namespace,
				},
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{
						{
							Name:       "raw-foo-2-predictor",
							Protocol:   "TCP",
							Port:       80,
							TargetPort: intstr.IntOrString{Type: 0, IntVal: 8080, StrVal: ""},
						},
					},
					Type:            "ClusterIP",
					SessionAffinity: "None",
					Selector: map[string]string{
						"app": "isvc.raw-foo-2-predictor",
					},
				},
			}
			actualService.Spec.ClusterIP = ""
			actualService.Spec.ClusterIPs = nil
			actualService.Spec.IPFamilies = nil
			actualService.Spec.IPFamilyPolicy = nil
			actualService.Spec.InternalTrafficPolicy = nil
			Expect(actualService.Spec).To(gomega.Equal(expectedService.Spec))

			//check isvc status
			updatedDeployment := actualDeployment.DeepCopy()
			updatedDeployment.Status.Conditions = []appsv1.DeploymentCondition{
				{
					Type:   appsv1.DeploymentAvailable,
					Status: v1.ConditionTrue,
				},
			}
			Expect(k8sClient.Status().Update(context.TODO(), updatedDeployment)).NotTo(gomega.HaveOccurred())

			//check ingress
			pathType := netv1.PathTypePrefix
			actualIngress := &netv1.Ingress{}
			predictorIngressKey := types.NamespacedName{Name: serviceKey.Name,
				Namespace: serviceKey.Namespace}
			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorIngressKey, actualIngress) }, timeout).
				Should(Succeed())
			expectedIngress := netv1.Ingress{
				Spec: netv1.IngressSpec{
					Rules: []netv1.IngressRule{
						{
							Host: "raw-foo-2-default.example.com",
							IngressRuleValue: netv1.IngressRuleValue{
								HTTP: &netv1.HTTPIngressRuleValue{
									Paths: []netv1.HTTPIngressPath{
										{
											Path:     "/",
											PathType: &pathType,
											Backend: netv1.IngressBackend{
												Service: &netv1.IngressServiceBackend{
													Name: "raw-foo-2-predictor",
													Port: netv1.ServiceBackendPort{
														Number: 80,
													},
												},
											},
										},
									},
								},
							},
						},
						{
							Host: "raw-foo-2-predictor-default.example.com",
							IngressRuleValue: netv1.IngressRuleValue{
								HTTP: &netv1.HTTPIngressRuleValue{
									Paths: []netv1.HTTPIngressPath{
										{
											Path:     "/",
											PathType: &pathType,
											Backend: netv1.IngressBackend{
												Service: &netv1.IngressServiceBackend{
													Name: "raw-foo-2-predictor",
													Port: netv1.ServiceBackendPort{
														Number: 80,
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			}
			Expect(actualIngress.Spec).To(gomega.Equal(expectedIngress.Spec))
			// verify if InferenceService status is updated
			expectedIsvcStatus := v1beta1.InferenceServiceStatus{
				Status: duckv1.Status{
					Conditions: duckv1.Conditions{
						{
							Type:   v1beta1.IngressReady,
							Status: "True",
						},
						{
							Type:   v1beta1.PredictorReady,
							Status: "True",
						},
						{
							Type:   apis.ConditionReady,
							Status: "True",
						},
					},
				},
				URL: &apis.URL{
					Scheme: "http",
					Host:   "raw-foo-2-default.example.com",
				},
				Address: &duckv1.Addressable{
					URL: &apis.URL{
						Scheme: "http",
						Host:   fmt.Sprintf("%s-predictor.%s.svc.cluster.local", serviceKey.Name, serviceKey.Namespace),
					},
				},
				Components: map[v1beta1.ComponentType]v1beta1.ComponentStatusSpec{
					v1beta1.PredictorComponent: {
						LatestCreatedRevision: "",
						URL: &apis.URL{
							Scheme: "http",
							Host:   "raw-foo-2-predictor-default.example.com",
						},
					},
				},
				ModelStatus: v1beta1.ModelStatus{
					TransitionStatus:    "InProgress",
					ModelRevisionStates: &v1beta1.ModelRevisionStates{TargetModelState: "Pending"},
				},
			}
			Eventually(func() string {
				isvc := &v1beta1.InferenceService{}
				if err := k8sClient.Get(context.TODO(), serviceKey, isvc); err != nil {
					return err.Error()
				}
				return cmp.Diff(&expectedIsvcStatus, &isvc.Status, cmpopts.IgnoreTypes(apis.VolatileTime{}))
			}, timeout).Should(gomega.BeEmpty())

			//check HPA is not created
			actualHPA := &autoscalingv2.HorizontalPodAutoscaler{}
			predictorHPAKey := types.NamespacedName{Name: constants.DefaultPredictorServiceName(serviceKey.Name),
				Namespace: serviceKey.Namespace}
			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorHPAKey, actualHPA) }, timeout).
				Should(HaveOccurred())
		})
		It("Should have no ingress created if labeled as cluster-local", func() {
			By("By creating a new InferenceService")
			// Create configmap
			var configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      constants.InferenceServiceConfigMapName,
					Namespace: constants.KServeNamespace,
				},
				Data: configs,
			}
			Expect(k8sClient.Create(context.TODO(), configMap)).NotTo(HaveOccurred())
			defer k8sClient.Delete(context.TODO(), configMap)
			// Create ServingRuntime
			servingRuntime := &v1alpha1.ServingRuntime{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tf-serving-raw",
					Namespace: "default",
				},
				Spec: v1alpha1.ServingRuntimeSpec{
					SupportedModelFormats: []v1alpha1.SupportedModelFormat{
						{
							Name:       "tensorflow",
							Version:    proto.String("1"),
							AutoSelect: proto.Bool(true),
						},
					},
					ServingRuntimePodSpec: v1alpha1.ServingRuntimePodSpec{
						Containers: []v1.Container{
							{
								Name:    "kserve-container",
								Image:   "tensorflow/serving:1.14.0",
								Command: []string{"/usr/bin/tensorflow_model_server"},
								Args: []string{
									"--port=9000",
									"--rest_api_port=8080",
									"--model_base_path=/mnt/models",
									"--rest_api_timeout_in_ms=60000",
								},
								Resources: defaultResource,
							},
						},
					},
					Disabled: proto.Bool(false),
				},
			}
			k8sClient.Create(context.TODO(), servingRuntime)
			defer k8sClient.Delete(context.TODO(), servingRuntime)
			serviceName := "raw-cluster-local"
			var expectedRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: serviceName, Namespace: "default"}}
			var serviceKey = expectedRequest.NamespacedName
			var storageUri = "s3://test/mnist/export"
			ctx := context.Background()
			isvc := &v1beta1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceKey.Name,
					Namespace: serviceKey.Namespace,
					Annotations: map[string]string{
						"serving.kserve.io/deploymentMode":              "RawDeployment",
						"serving.kserve.io/autoscalerClass":             "hpa",
						"serving.kserve.io/metrics":                     "cpu",
						"serving.kserve.io/targetUtilizationPercentage": "75",
					},
					Labels: map[string]string{
						"networking.kserve.io/visibility": "cluster-local",
					},
				},
				Spec: v1beta1.InferenceServiceSpec{
					Predictor: v1beta1.PredictorSpec{
						ComponentExtensionSpec: v1beta1.ComponentExtensionSpec{
							MinReplicas: v1beta1.GetIntReference(1),
							MaxReplicas: 3,
						},
						Tensorflow: &v1beta1.TFServingSpec{
							PredictorExtensionSpec: v1beta1.PredictorExtensionSpec{
								StorageURI:     &storageUri,
								RuntimeVersion: proto.String("1.14.0"),
								Container: v1.Container{
									Name:      constants.InferenceServiceContainerName,
									Resources: defaultResource,
								},
							},
						},
					},
				},
			}
			isvc.DefaultInferenceService(nil, nil)
			Expect(k8sClient.Create(ctx, isvc)).Should(Succeed())

			inferenceService := &v1beta1.InferenceService{}

			Eventually(func() bool {
				err := k8sClient.Get(ctx, serviceKey, inferenceService)
				if err != nil {
					return false
				}
				return true
			}, timeout, interval).Should(BeTrue())
			actualIngress := &netv1.Ingress{}
			predictorIngressKey := types.NamespacedName{Name: serviceKey.Name,
				Namespace: serviceKey.Namespace}
			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorIngressKey, actualIngress) }, timeout).
				ShouldNot(Succeed())
		})
	})
	Context("When creating inference service with raw kube predictor and empty ingressClassName", func() {
		configs := map[string]string{
			"explainers": `{
               "alibi": {
                  "image": "kfserving/alibi-explainer",
			      "defaultImageVersion": "latest"
               }
            }`,
			"ingress": `{
               "ingressGateway": "knative-serving/knative-ingress-gateway",
               "ingressService": "test-destination",
               "localGateway": "knative-serving/knative-local-gateway",
               "localGatewayService": "knative-local-gateway.istio-system.svc.cluster.local",
               "ingressDomain": "example.com"
            }`,
		}

		It("Should have ingress/service/deployment/hpa created", func() {
			By("By creating a new InferenceService")
			// Create configmap
			var configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      constants.InferenceServiceConfigMapName,
					Namespace: constants.KServeNamespace,
				},
				Data: configs,
			}
			Expect(k8sClient.Create(context.TODO(), configMap)).NotTo(HaveOccurred())
			defer k8sClient.Delete(context.TODO(), configMap)
			// Create ServingRuntime
			servingRuntime := &v1alpha1.ServingRuntime{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tf-serving-raw",
					Namespace: "default",
				},
				Spec: v1alpha1.ServingRuntimeSpec{
					SupportedModelFormats: []v1alpha1.SupportedModelFormat{
						{
							Name:       "tensorflow",
							Version:    proto.String("1"),
							AutoSelect: proto.Bool(true),
						},
					},
					ServingRuntimePodSpec: v1alpha1.ServingRuntimePodSpec{
						Containers: []v1.Container{
							{
								Name:    "kserve-container",
								Image:   "tensorflow/serving:1.14.0",
								Command: []string{"/usr/bin/tensorflow_model_server"},
								Args: []string{
									"--port=9000",
									"--rest_api_port=8080",
									"--model_base_path=/mnt/models",
									"--rest_api_timeout_in_ms=60000",
								},
								Resources: defaultResource,
							},
						},
					},
					Disabled: proto.Bool(false),
				},
			}
			k8sClient.Create(context.TODO(), servingRuntime)
			defer k8sClient.Delete(context.TODO(), servingRuntime)
			// Create InferenceService
			serviceName := "raw-foo-no-ingress-class"
			var expectedRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: serviceName, Namespace: "default"}}
			var serviceKey = expectedRequest.NamespacedName
			var storageUri = "s3://test/mnist/export"
			ctx := context.Background()
			isvc := &v1beta1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceKey.Name,
					Namespace: serviceKey.Namespace,
					Annotations: map[string]string{
						"serving.kserve.io/deploymentMode":              "RawDeployment",
						"serving.kserve.io/autoscalerClass":             "hpa",
						"serving.kserve.io/metrics":                     "cpu",
						"serving.kserve.io/targetUtilizationPercentage": "75",
					},
				},
				Spec: v1beta1.InferenceServiceSpec{
					Predictor: v1beta1.PredictorSpec{
						ComponentExtensionSpec: v1beta1.ComponentExtensionSpec{
							MinReplicas: v1beta1.GetIntReference(1),
							MaxReplicas: 3,
						},
						Tensorflow: &v1beta1.TFServingSpec{
							PredictorExtensionSpec: v1beta1.PredictorExtensionSpec{
								StorageURI:     &storageUri,
								RuntimeVersion: proto.String("1.14.0"),
								Container: v1.Container{
									Name:      constants.InferenceServiceContainerName,
									Resources: defaultResource,
								},
							},
						},
					},
				},
			}
			isvc.DefaultInferenceService(nil, nil)
			Expect(k8sClient.Create(ctx, isvc)).Should(Succeed())

			inferenceService := &v1beta1.InferenceService{}

			Eventually(func() bool {
				err := k8sClient.Get(ctx, serviceKey, inferenceService)
				if err != nil {
					return false
				}
				return true
			}, timeout, interval).Should(BeTrue())

			actualDeployment := &appsv1.Deployment{}
			predictorDeploymentKey := types.NamespacedName{Name: constants.PredictorServiceName(serviceKey.Name),
				Namespace: serviceKey.Namespace}
			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorDeploymentKey, actualDeployment) }, timeout).
				Should(Succeed())
			var replicas int32 = 1
			var revisionHistory int32 = 10
			var progressDeadlineSeconds int32 = 600
			var gracePeriod int64 = 30
			expectedDeployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      predictorDeploymentKey.Name,
					Namespace: predictorDeploymentKey.Namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "isvc." + predictorDeploymentKey.Name,
						},
					},
					Template: v1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Name:      predictorDeploymentKey.Name,
							Namespace: "default",
							Labels: map[string]string{
								"app":                                 "isvc." + predictorDeploymentKey.Name,
								constants.KServiceComponentLabel:      constants.Predictor.String(),
								constants.InferenceServicePodLabelKey: serviceName,
							},
							Annotations: map[string]string{
								constants.StorageInitializerSourceUriInternalAnnotationKey: *isvc.Spec.Predictor.Model.StorageURI,
								"serving.kserve.io/deploymentMode":                         "RawDeployment",
								"serving.kserve.io/autoscalerClass":                        "hpa",
								"serving.kserve.io/metrics":                                "cpu",
								"serving.kserve.io/targetUtilizationPercentage":            "75",
							},
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Image: "tensorflow/serving:" +
										*isvc.Spec.Predictor.Model.RuntimeVersion,
									Name:    constants.InferenceServiceContainerName,
									Command: []string{v1beta1.TensorflowEntrypointCommand},
									Args: []string{
										"--port=" + v1beta1.TensorflowServingGRPCPort,
										"--rest_api_port=" + v1beta1.TensorflowServingRestPort,
										"--model_base_path=" + constants.DefaultModelLocalMountPath,
										"--rest_api_timeout_in_ms=60000",
									},
									Resources: defaultResource,
									ReadinessProbe: &v1.Probe{
										ProbeHandler: v1.ProbeHandler{
											TCPSocket: &v1.TCPSocketAction{
												Port: intstr.IntOrString{
													IntVal: 8080,
												},
											},
										},
										InitialDelaySeconds: 0,
										TimeoutSeconds:      1,
										PeriodSeconds:       10,
										SuccessThreshold:    1,
										FailureThreshold:    3,
									},
									TerminationMessagePath:   "/dev/termination-log",
									TerminationMessagePolicy: "File",
									ImagePullPolicy:          "IfNotPresent",
								},
							},
							SchedulerName:                 "default-scheduler",
							RestartPolicy:                 "Always",
							TerminationGracePeriodSeconds: &gracePeriod,
							DNSPolicy:                     "ClusterFirst",
							SecurityContext: &v1.PodSecurityContext{
								SELinuxOptions:      nil,
								WindowsOptions:      nil,
								RunAsUser:           nil,
								RunAsGroup:          nil,
								RunAsNonRoot:        nil,
								SupplementalGroups:  nil,
								FSGroup:             nil,
								Sysctls:             nil,
								FSGroupChangePolicy: nil,
								SeccompProfile:      nil,
							},
							ServiceAccountName:       constants.KserveServiceAccountName,
							DeprecatedServiceAccount: constants.KserveServiceAccountName,
						},
					},
					Strategy: appsv1.DeploymentStrategy{
						Type: "RollingUpdate",
						RollingUpdate: &appsv1.RollingUpdateDeployment{
							MaxUnavailable: &intstr.IntOrString{Type: 1, IntVal: 0, StrVal: "25%"},
							MaxSurge:       &intstr.IntOrString{Type: 1, IntVal: 0, StrVal: "25%"},
						},
					},
					RevisionHistoryLimit:    &revisionHistory,
					ProgressDeadlineSeconds: &progressDeadlineSeconds,
				},
			}
			Expect(actualDeployment.Spec).To(gomega.Equal(expectedDeployment.Spec))

			//check service
			actualService := &v1.Service{}
			predictorServiceKey := types.NamespacedName{Name: constants.PredictorServiceName(serviceKey.Name),
				Namespace: serviceKey.Namespace}
			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorServiceKey, actualService) }, timeout).
				Should(Succeed())

			expectedService := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      predictorServiceKey.Name,
					Namespace: predictorServiceKey.Namespace,
				},
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{
						{
							Name:       constants.PredictorServiceName(serviceName),
							Protocol:   "TCP",
							Port:       80,
							TargetPort: intstr.IntOrString{Type: 0, IntVal: 8080, StrVal: ""},
						},
					},
					Type:            "ClusterIP",
					SessionAffinity: "None",
					Selector: map[string]string{
						"app": fmt.Sprintf("isvc.%s", constants.PredictorServiceName(serviceName)),
					},
				},
			}
			actualService.Spec.ClusterIP = ""
			actualService.Spec.ClusterIPs = nil
			actualService.Spec.IPFamilies = nil
			actualService.Spec.IPFamilyPolicy = nil
			actualService.Spec.InternalTrafficPolicy = nil
			Expect(actualService.Spec).To(gomega.Equal(expectedService.Spec))

			//check isvc status
			updatedDeployment := actualDeployment.DeepCopy()
			updatedDeployment.Status.Conditions = []appsv1.DeploymentCondition{
				{
					Type:   appsv1.DeploymentAvailable,
					Status: v1.ConditionTrue,
				},
			}
			Expect(k8sClient.Status().Update(context.TODO(), updatedDeployment)).NotTo(gomega.HaveOccurred())

			//check ingress
			pathType := netv1.PathTypePrefix
			actualIngress := &netv1.Ingress{}
			predictorIngressKey := types.NamespacedName{Name: serviceKey.Name,
				Namespace: serviceKey.Namespace}
			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorIngressKey, actualIngress) }, timeout).
				Should(Succeed())
			expectedIngress := netv1.Ingress{
				Spec: netv1.IngressSpec{
					Rules: []netv1.IngressRule{
						{
							Host: fmt.Sprintf("%s-default.example.com", serviceName),
							IngressRuleValue: netv1.IngressRuleValue{
								HTTP: &netv1.HTTPIngressRuleValue{
									Paths: []netv1.HTTPIngressPath{
										{
											Path:     "/",
											PathType: &pathType,
											Backend: netv1.IngressBackend{
												Service: &netv1.IngressServiceBackend{
													Name: fmt.Sprintf("%s-predictor", serviceName),
													Port: netv1.ServiceBackendPort{
														Number: 80,
													},
												},
											},
										},
									},
								},
							},
						},
						{
							Host: fmt.Sprintf("%s-predictor-default.example.com", serviceName),
							IngressRuleValue: netv1.IngressRuleValue{
								HTTP: &netv1.HTTPIngressRuleValue{
									Paths: []netv1.HTTPIngressPath{
										{
											Path:     "/",
											PathType: &pathType,
											Backend: netv1.IngressBackend{
												Service: &netv1.IngressServiceBackend{
													Name: fmt.Sprintf("%s-predictor", serviceName),
													Port: netv1.ServiceBackendPort{
														Number: 80,
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			}
			Expect(actualIngress.Spec).To(gomega.Equal(expectedIngress.Spec))
			// verify if InferenceService status is updated
			expectedIsvcStatus := v1beta1.InferenceServiceStatus{
				Status: duckv1.Status{
					Conditions: duckv1.Conditions{
						{
							Type:   v1beta1.IngressReady,
							Status: "True",
						},
						{
							Type:   v1beta1.PredictorReady,
							Status: "True",
						},
						{
							Type:   apis.ConditionReady,
							Status: "True",
						},
					},
				},
				URL: &apis.URL{
					Scheme: "http",
					Host:   fmt.Sprintf("%s-default.example.com", serviceName),
				},
				Address: &duckv1.Addressable{
					URL: &apis.URL{
						Scheme: "http",
						Host:   fmt.Sprintf("%s-predictor.%s.svc.cluster.local", serviceKey.Name, serviceKey.Namespace),
					},
				},
				Components: map[v1beta1.ComponentType]v1beta1.ComponentStatusSpec{
					v1beta1.PredictorComponent: {
						LatestCreatedRevision: "",
						URL: &apis.URL{
							Scheme: "http",
							Host:   fmt.Sprintf("%s-predictor-default.example.com", serviceName),
						},
					},
				},
				ModelStatus: v1beta1.ModelStatus{
					TransitionStatus:    "InProgress",
					ModelRevisionStates: &v1beta1.ModelRevisionStates{TargetModelState: "Pending"},
				},
			}
			Eventually(func() string {
				isvc := &v1beta1.InferenceService{}
				if err := k8sClient.Get(context.TODO(), serviceKey, isvc); err != nil {
					return err.Error()
				}
				return cmp.Diff(&expectedIsvcStatus, &isvc.Status, cmpopts.IgnoreTypes(apis.VolatileTime{}))
			}, timeout).Should(gomega.BeEmpty())

			//check HPA
			var minReplicas int32 = 1
			var maxReplicas int32 = 3
			var cpuUtilization int32 = 75
			var stabilizationWindowSeconds int32 = 0
			selectPolicy := autoscalingv2.MaxChangePolicySelect
			actualHPA := &autoscalingv2.HorizontalPodAutoscaler{}
			predictorHPAKey := types.NamespacedName{Name: constants.PredictorServiceName(serviceKey.Name),
				Namespace: serviceKey.Namespace}
			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorHPAKey, actualHPA) }, timeout).
				Should(Succeed())
			expectedHPA := &autoscalingv2.HorizontalPodAutoscaler{
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       constants.PredictorServiceName(serviceKey.Name),
					},
					MinReplicas: &minReplicas,
					MaxReplicas: maxReplicas,
					Metrics: []autoscalingv2.MetricSpec{
						{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								Name: v1.ResourceCPU,
								Target: autoscalingv2.MetricTarget{
									Type:               "Utilization",
									AverageUtilization: &cpuUtilization,
								},
							},
						},
					},
					Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
						ScaleUp: &autoscalingv2.HPAScalingRules{
							StabilizationWindowSeconds: &stabilizationWindowSeconds,
							SelectPolicy:               &selectPolicy,
							Policies: []autoscalingv2.HPAScalingPolicy{
								{
									Type:          "Pods",
									Value:         4,
									PeriodSeconds: 15,
								},
								{
									Type:          "Percent",
									Value:         100,
									PeriodSeconds: 15,
								},
							},
						},
						ScaleDown: &autoscalingv2.HPAScalingRules{
							StabilizationWindowSeconds: nil,
							SelectPolicy:               &selectPolicy,
							Policies: []autoscalingv2.HPAScalingPolicy{
								{
									Type:          "Percent",
									Value:         100,
									PeriodSeconds: 15,
								},
							},
						},
					},
				},
			}
			Expect(actualHPA.Spec).To(gomega.Equal(expectedHPA.Spec))
		})
	})
	Context("When creating inference service with raw kube predictor with domain template", func() {
		configs := map[string]string{
			"explainers": `{
               "alibi": {
                  "image": "kfserving/alibi-explainer",
			      "defaultImageVersion": "latest"
               }
            }`,
			"ingress": `{
               "ingressGateway": "knative-serving/knative-ingress-gateway",
               "ingressService": "test-destination",
               "localGateway": "knative-serving/knative-local-gateway",
               "localGatewayService": "knative-local-gateway.istio-system.svc.cluster.local",
               "ingressDomain": "example.com",
               "domainTemplate": "{{ .Name }}.{{ .Namespace }}.{{ .IngressDomain }}"
            }`,
		}

		It("Should have ingress/service/deployment/hpa created", func() {
			By("By creating a new InferenceService")
			// Create configmap
			var configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      constants.InferenceServiceConfigMapName,
					Namespace: constants.KServeNamespace,
				},
				Data: configs,
			}
			Expect(k8sClient.Create(context.TODO(), configMap)).NotTo(HaveOccurred())
			defer k8sClient.Delete(context.TODO(), configMap)
			// Create ServingRuntime
			servingRuntime := &v1alpha1.ServingRuntime{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tf-serving-raw",
					Namespace: "default",
				},
				Spec: v1alpha1.ServingRuntimeSpec{
					SupportedModelFormats: []v1alpha1.SupportedModelFormat{
						{
							Name:       "tensorflow",
							Version:    proto.String("1"),
							AutoSelect: proto.Bool(true),
						},
					},
					ServingRuntimePodSpec: v1alpha1.ServingRuntimePodSpec{
						Containers: []v1.Container{
							{
								Name:    "kserve-container",
								Image:   "tensorflow/serving:1.14.0",
								Command: []string{"/usr/bin/tensorflow_model_server"},
								Args: []string{
									"--port=9000",
									"--rest_api_port=8080",
									"--model_base_path=/mnt/models",
									"--rest_api_timeout_in_ms=60000",
								},
								Resources: defaultResource,
							},
						},
					},
					Disabled: proto.Bool(false),
				},
			}
			k8sClient.Create(context.TODO(), servingRuntime)
			defer k8sClient.Delete(context.TODO(), servingRuntime)
			// Create InferenceService
			serviceName := "model"
			var expectedRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: serviceName, Namespace: "default"}}
			var serviceKey = expectedRequest.NamespacedName
			var storageUri = "s3://test/mnist/export"
			ctx := context.Background()
			isvc := &v1beta1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceKey.Name,
					Namespace: serviceKey.Namespace,
					Annotations: map[string]string{
						"serving.kserve.io/deploymentMode":              "RawDeployment",
						"serving.kserve.io/autoscalerClass":             "hpa",
						"serving.kserve.io/metrics":                     "cpu",
						"serving.kserve.io/targetUtilizationPercentage": "75",
					},
				},
				Spec: v1beta1.InferenceServiceSpec{
					Predictor: v1beta1.PredictorSpec{
						ComponentExtensionSpec: v1beta1.ComponentExtensionSpec{
							MinReplicas: v1beta1.GetIntReference(1),
							MaxReplicas: 3,
						},
						Tensorflow: &v1beta1.TFServingSpec{
							PredictorExtensionSpec: v1beta1.PredictorExtensionSpec{
								StorageURI:     &storageUri,
								RuntimeVersion: proto.String("1.14.0"),
								Container: v1.Container{
									Name:      constants.InferenceServiceContainerName,
									Resources: defaultResource,
								},
							},
						},
					},
				},
			}
			isvc.DefaultInferenceService(nil, nil)
			Expect(k8sClient.Create(ctx, isvc)).Should(Succeed())

			inferenceService := &v1beta1.InferenceService{}

			Eventually(func() bool {
				err := k8sClient.Get(ctx, serviceKey, inferenceService)
				if err != nil {
					return false
				}
				return true
			}, timeout, interval).Should(BeTrue())

			actualDeployment := &appsv1.Deployment{}
			predictorDeploymentKey := types.NamespacedName{Name: constants.PredictorServiceName(serviceKey.Name),
				Namespace: serviceKey.Namespace}
			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorDeploymentKey, actualDeployment) }, timeout).
				Should(Succeed())
			var replicas int32 = 1
			var revisionHistory int32 = 10
			var progressDeadlineSeconds int32 = 600
			var gracePeriod int64 = 30
			expectedDeployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      predictorDeploymentKey.Name,
					Namespace: predictorDeploymentKey.Namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "isvc." + predictorDeploymentKey.Name,
						},
					},
					Template: v1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Name:      predictorDeploymentKey.Name,
							Namespace: "default",
							Labels: map[string]string{
								"app":                                 "isvc." + predictorDeploymentKey.Name,
								constants.KServiceComponentLabel:      constants.Predictor.String(),
								constants.InferenceServicePodLabelKey: serviceName,
							},
							Annotations: map[string]string{
								constants.StorageInitializerSourceUriInternalAnnotationKey: *isvc.Spec.Predictor.Model.StorageURI,
								"serving.kserve.io/deploymentMode":                         "RawDeployment",
								"serving.kserve.io/autoscalerClass":                        "hpa",
								"serving.kserve.io/metrics":                                "cpu",
								"serving.kserve.io/targetUtilizationPercentage":            "75",
							},
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Image: "tensorflow/serving:" +
										*isvc.Spec.Predictor.Model.RuntimeVersion,
									Name:    constants.InferenceServiceContainerName,
									Command: []string{v1beta1.TensorflowEntrypointCommand},
									Args: []string{
										"--port=" + v1beta1.TensorflowServingGRPCPort,
										"--rest_api_port=" + v1beta1.TensorflowServingRestPort,
										"--model_base_path=" + constants.DefaultModelLocalMountPath,
										"--rest_api_timeout_in_ms=60000",
									},
									Resources: defaultResource,
									ReadinessProbe: &v1.Probe{
										ProbeHandler: v1.ProbeHandler{
											TCPSocket: &v1.TCPSocketAction{
												Port: intstr.IntOrString{
													IntVal: 8080,
												},
											},
										},
										InitialDelaySeconds: 0,
										TimeoutSeconds:      1,
										PeriodSeconds:       10,
										SuccessThreshold:    1,
										FailureThreshold:    3,
									},
									TerminationMessagePath:   "/dev/termination-log",
									TerminationMessagePolicy: "File",
									ImagePullPolicy:          "IfNotPresent",
								},
							},
							SchedulerName:                 "default-scheduler",
							RestartPolicy:                 "Always",
							TerminationGracePeriodSeconds: &gracePeriod,
							DNSPolicy:                     "ClusterFirst",
							SecurityContext: &v1.PodSecurityContext{
								SELinuxOptions:      nil,
								WindowsOptions:      nil,
								RunAsUser:           nil,
								RunAsGroup:          nil,
								RunAsNonRoot:        nil,
								SupplementalGroups:  nil,
								FSGroup:             nil,
								Sysctls:             nil,
								FSGroupChangePolicy: nil,
								SeccompProfile:      nil,
							},
							ServiceAccountName:       constants.KserveServiceAccountName,
							DeprecatedServiceAccount: constants.KserveServiceAccountName,
						},
					},
					Strategy: appsv1.DeploymentStrategy{
						Type: "RollingUpdate",
						RollingUpdate: &appsv1.RollingUpdateDeployment{
							MaxUnavailable: &intstr.IntOrString{Type: 1, IntVal: 0, StrVal: "25%"},
							MaxSurge:       &intstr.IntOrString{Type: 1, IntVal: 0, StrVal: "25%"},
						},
					},
					RevisionHistoryLimit:    &revisionHistory,
					ProgressDeadlineSeconds: &progressDeadlineSeconds,
				},
			}
			Expect(actualDeployment.Spec).To(gomega.Equal(expectedDeployment.Spec))

			//check service
			actualService := &v1.Service{}
			predictorServiceKey := types.NamespacedName{Name: constants.PredictorServiceName(serviceKey.Name),
				Namespace: serviceKey.Namespace}
			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorServiceKey, actualService) }, timeout).
				Should(Succeed())

			expectedService := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      predictorServiceKey.Name,
					Namespace: predictorServiceKey.Namespace,
				},
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{
						{
							Name:       constants.PredictorServiceName(serviceName),
							Protocol:   "TCP",
							Port:       80,
							TargetPort: intstr.IntOrString{Type: 0, IntVal: 8080, StrVal: ""},
						},
					},
					Type:            "ClusterIP",
					SessionAffinity: "None",
					Selector: map[string]string{
						"app": fmt.Sprintf("isvc.%s", constants.PredictorServiceName(serviceName)),
					},
				},
			}
			actualService.Spec.ClusterIP = ""
			actualService.Spec.ClusterIPs = nil
			actualService.Spec.IPFamilies = nil
			actualService.Spec.IPFamilyPolicy = nil
			actualService.Spec.InternalTrafficPolicy = nil
			Expect(actualService.Spec).To(gomega.Equal(expectedService.Spec))

			//check isvc status
			updatedDeployment := actualDeployment.DeepCopy()
			updatedDeployment.Status.Conditions = []appsv1.DeploymentCondition{
				{
					Type:   appsv1.DeploymentAvailable,
					Status: v1.ConditionTrue,
				},
			}
			Expect(k8sClient.Status().Update(context.TODO(), updatedDeployment)).NotTo(gomega.HaveOccurred())

			//check ingress
			pathType := netv1.PathTypePrefix
			actualIngress := &netv1.Ingress{}
			predictorIngressKey := types.NamespacedName{Name: serviceKey.Name,
				Namespace: serviceKey.Namespace}
			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorIngressKey, actualIngress) }, timeout).
				Should(Succeed())
			expectedIngress := netv1.Ingress{
				Spec: netv1.IngressSpec{
					Rules: []netv1.IngressRule{
						{
							Host: fmt.Sprintf("%s.%s.%s", serviceName, serviceKey.Namespace, domain),
							IngressRuleValue: netv1.IngressRuleValue{
								HTTP: &netv1.HTTPIngressRuleValue{
									Paths: []netv1.HTTPIngressPath{
										{
											Path:     "/",
											PathType: &pathType,
											Backend: netv1.IngressBackend{
												Service: &netv1.IngressServiceBackend{
													Name: fmt.Sprintf("%s-predictor", serviceName),
													Port: netv1.ServiceBackendPort{
														Number: 80,
													},
												},
											},
										},
									},
								},
							},
						},
						{
							Host: fmt.Sprintf("%s-predictor.%s.%s", serviceName, serviceKey.Namespace, domain),
							IngressRuleValue: netv1.IngressRuleValue{
								HTTP: &netv1.HTTPIngressRuleValue{
									Paths: []netv1.HTTPIngressPath{
										{
											Path:     "/",
											PathType: &pathType,
											Backend: netv1.IngressBackend{
												Service: &netv1.IngressServiceBackend{
													Name: fmt.Sprintf("%s-predictor", serviceName),
													Port: netv1.ServiceBackendPort{
														Number: 80,
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			}
			Expect(actualIngress.Spec).To(gomega.Equal(expectedIngress.Spec))
			// verify if InferenceService status is updated
			expectedIsvcStatus := v1beta1.InferenceServiceStatus{
				Status: duckv1.Status{
					Conditions: duckv1.Conditions{
						{
							Type:   v1beta1.IngressReady,
							Status: "True",
						},
						{
							Type:   v1beta1.PredictorReady,
							Status: "True",
						},
						{
							Type:   apis.ConditionReady,
							Status: "True",
						},
					},
				},
				URL: &apis.URL{
					Scheme: "http",
					Host:   fmt.Sprintf("%s.%s.%s", serviceName, serviceKey.Namespace, domain),
				},
				Address: &duckv1.Addressable{
					URL: &apis.URL{
						Scheme: "http",
						Host:   fmt.Sprintf("%s-predictor.%s.svc.cluster.local", serviceKey.Name, serviceKey.Namespace),
					},
				},
				Components: map[v1beta1.ComponentType]v1beta1.ComponentStatusSpec{
					v1beta1.PredictorComponent: {
						LatestCreatedRevision: "",
						URL: &apis.URL{
							Scheme: "http",
							Host:   fmt.Sprintf("%s-predictor.%s.%s", serviceName, serviceKey.Namespace, domain),
						},
					},
				},
				ModelStatus: v1beta1.ModelStatus{
					TransitionStatus:    "InProgress",
					ModelRevisionStates: &v1beta1.ModelRevisionStates{TargetModelState: "Pending"},
				},
			}
			Eventually(func() string {
				isvc := &v1beta1.InferenceService{}
				if err := k8sClient.Get(context.TODO(), serviceKey, isvc); err != nil {
					return err.Error()
				}
				return cmp.Diff(&expectedIsvcStatus, &isvc.Status, cmpopts.IgnoreTypes(apis.VolatileTime{}))
			}, timeout).Should(gomega.BeEmpty())

			//check HPA
			var minReplicas int32 = 1
			var maxReplicas int32 = 3
			var cpuUtilization int32 = 75
			var stabilizationWindowSeconds int32 = 0
			selectPolicy := autoscalingv2.MaxChangePolicySelect
			actualHPA := &autoscalingv2.HorizontalPodAutoscaler{}
			predictorHPAKey := types.NamespacedName{Name: constants.PredictorServiceName(serviceKey.Name),
				Namespace: serviceKey.Namespace}
			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorHPAKey, actualHPA) }, timeout).
				Should(Succeed())
			expectedHPA := &autoscalingv2.HorizontalPodAutoscaler{
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       constants.PredictorServiceName(serviceKey.Name),
					},
					MinReplicas: &minReplicas,
					MaxReplicas: maxReplicas,
					Metrics: []autoscalingv2.MetricSpec{
						{
							Type: autoscalingv2.ResourceMetricSourceType,
							Resource: &autoscalingv2.ResourceMetricSource{
								Name: v1.ResourceCPU,
								Target: autoscalingv2.MetricTarget{
									Type:               "Utilization",
									AverageUtilization: &cpuUtilization,
								},
							},
						},
					},
					Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
						ScaleUp: &autoscalingv2.HPAScalingRules{
							StabilizationWindowSeconds: &stabilizationWindowSeconds,
							SelectPolicy:               &selectPolicy,
							Policies: []autoscalingv2.HPAScalingPolicy{
								{
									Type:          "Pods",
									Value:         4,
									PeriodSeconds: 15,
								},
								{
									Type:          "Percent",
									Value:         100,
									PeriodSeconds: 15,
								},
							},
						},
						ScaleDown: &autoscalingv2.HPAScalingRules{
							StabilizationWindowSeconds: nil,
							SelectPolicy:               &selectPolicy,
							Policies: []autoscalingv2.HPAScalingPolicy{
								{
									Type:          "Percent",
									Value:         100,
									PeriodSeconds: 15,
								},
							},
						},
					},
				},
			}
			Expect(actualHPA.Spec).To(gomega.Equal(expectedHPA.Spec))
		})
	})
	Context("When creating an inferenceservice with raw kube predictor and ODH auth enabled", func() {
		configs := map[string]string{
			"explainers": `{
				"alibi": {
					"image": "kserve/alibi-explainer",
					"defaultImageVersion": "latest"
				}
			}`,
			"ingress": `{
				"ingressGateway": "knative-serving/knative-ingress-gateway",
				"ingressService": "test-destination",
				"localGateway": "knative-serving/knative-local-gateway",
				"localGatewayService": "knative-local-gateway.istio-system.svc.cluster.local"
			}`,
			"storageInitializer": `{
				"image" : "kserve/storage-initializer:latest",
				"memoryRequest": "100Mi",
				"memoryLimit": "1Gi",
				"cpuRequest": "100m",
				"cpuLimit": "1",
				"CaBundleConfigMapName": "",
				"caBundleVolumeMountPath": "/etc/ssl/custom-certs",
				"enableDirectPvcVolumeMount": false
			}`,
		}

		It("Should have ingress/service/deployment/hpa created", func() {
			By("By creating a new InferenceService")
			// Create configmap
			var configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      constants.InferenceServiceConfigMapName,
					Namespace: constants.KServeNamespace,
				},
				Data: configs,
			}
			Expect(k8sClient.Create(context.TODO(), configMap)).NotTo(HaveOccurred())
			defer k8sClient.Delete(context.TODO(), configMap)
			// Create ServingRuntime
			servingRuntime := &v1alpha1.ServingRuntime{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "tf-serving-raw",
					Namespace: "default",
				},
				Spec: v1alpha1.ServingRuntimeSpec{
					SupportedModelFormats: []v1alpha1.SupportedModelFormat{
						{
							Name:       "tensorflow",
							Version:    proto.String("1"),
							AutoSelect: proto.Bool(true),
						},
					},
					ServingRuntimePodSpec: v1alpha1.ServingRuntimePodSpec{
						Containers: []v1.Container{
							{
								Name:    "kserve-container",
								Image:   "tensorflow/serving:1.14.0",
								Command: []string{"/usr/bin/tensorflow_model_server"},
								Args: []string{
									"--port=9000",
									"--rest_api_port=8080",
									"--model_base_path=/mnt/models",
									"--rest_api_timeout_in_ms=60000",
								},
								Resources: defaultResource,
							},
						},
					},
					Disabled: proto.Bool(false),
				},
			}
			k8sClient.Create(context.TODO(), servingRuntime)
			defer k8sClient.Delete(context.TODO(), servingRuntime)
			serviceName := "raw-auth"
			var expectedRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: serviceName, Namespace: "default"}}
			var serviceKey = expectedRequest.NamespacedName
			var storageUri = "s3://test/mnist/export"
			ctx := context.Background()
			isvc := &v1beta1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceKey.Name,
					Namespace: serviceKey.Namespace,
					Annotations: map[string]string{
						"serving.kserve.io/deploymentMode": "RawDeployment",
					},
					Labels: map[string]string{
						constants.ODHKserveRawAuth: "true",
					},
				},
				Spec: v1beta1.InferenceServiceSpec{
					Predictor: v1beta1.PredictorSpec{
						ComponentExtensionSpec: v1beta1.ComponentExtensionSpec{
							MinReplicas: v1beta1.GetIntReference(1),
							MaxReplicas: 3,
						},
						Tensorflow: &v1beta1.TFServingSpec{
							PredictorExtensionSpec: v1beta1.PredictorExtensionSpec{
								StorageURI:     &storageUri,
								RuntimeVersion: proto.String("1.14.0"),
								Container: v1.Container{
									Name:      constants.InferenceServiceContainerName,
									Resources: defaultResource,
								},
							},
						},
					},
				},
			}
			isvc.DefaultInferenceService(nil, nil)
			Expect(k8sClient.Create(ctx, isvc)).Should(Succeed())

			inferenceService := &v1beta1.InferenceService{}

			Eventually(func() bool {
				err := k8sClient.Get(ctx, serviceKey, inferenceService)
				if err != nil {
					return false
				}
				return true
			}, timeout, interval).Should(BeTrue())

			actualDeployment := &appsv1.Deployment{}
			predictorDeploymentKey := types.NamespacedName{Name: constants.PredictorServiceName(serviceKey.Name),
				Namespace: serviceKey.Namespace}
			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorDeploymentKey, actualDeployment) }, timeout).
				Should(Succeed())
			var replicas int32 = 1
			var revisionHistory int32 = 10
			var progressDeadlineSeconds int32 = 600
			var gracePeriod int64 = 30
			expectedDeployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      predictorDeploymentKey.Name,
					Namespace: predictorDeploymentKey.Namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "isvc." + predictorDeploymentKey.Name,
						},
					},
					Template: v1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Name:      predictorDeploymentKey.Name,
							Namespace: "default",
							Labels: map[string]string{
								"app":                                 "isvc." + predictorDeploymentKey.Name,
								constants.KServiceComponentLabel:      constants.Predictor.String(),
								constants.InferenceServicePodLabelKey: serviceName,
								"serving.kserve.io/inferenceservice":  serviceName,
								constants.ODHKserveRawAuth:            "true",
							},
							Annotations: map[string]string{
								constants.StorageInitializerSourceUriInternalAnnotationKey: *isvc.Spec.Predictor.Model.StorageURI,
								"serving.kserve.io/deploymentMode":                         "RawDeployment",
								"service.beta.openshift.io/serving-cert-secret-name":       predictorDeploymentKey.Name,
							},
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Image: "tensorflow/serving:" +
										*isvc.Spec.Predictor.Model.RuntimeVersion,
									Name:    constants.InferenceServiceContainerName,
									Command: []string{v1beta1.TensorflowEntrypointCommand},
									Args: []string{
										"--port=" + v1beta1.TensorflowServingGRPCPort,
										"--rest_api_port=" + v1beta1.TensorflowServingRestPort,
										"--model_base_path=" + constants.DefaultModelLocalMountPath,
										"--rest_api_timeout_in_ms=60000",
									},
									Resources: defaultResource,
									ReadinessProbe: &v1.Probe{
										ProbeHandler: v1.ProbeHandler{
											TCPSocket: &v1.TCPSocketAction{
												Port: intstr.IntOrString{
													IntVal: 8080,
												},
											},
										},
										InitialDelaySeconds: 0,
										TimeoutSeconds:      1,
										PeriodSeconds:       10,
										SuccessThreshold:    1,
										FailureThreshold:    3,
									},
									TerminationMessagePath:   "/dev/termination-log",
									TerminationMessagePolicy: "File",
									ImagePullPolicy:          "IfNotPresent",
								},
								{
									Name:  "oauth-proxy",
									Image: constants.OauthProxyImage,
									Args: []string{
										`--https-address=:8443`,
										`--provider=openshift`,
										`--openshift-service-account=kserve-sa`,
										`--upstream=http://localhost:8080`,
										`--tls-cert=/etc/tls/private/tls.crt`,
										`--tls-key=/etc/tls/private/tls.key`,
										`--cookie-secret=SECRET`,
										`--openshift-delegate-urls={"/": {"namespace": "` + serviceKey.Namespace + `", "resource": "services", "verb": "get"}}`,
										`--openshift-sar={"namespace": "` + serviceKey.Namespace + `", "resource": "services", "verb": "get"}`,
										`--skip-auth-regex="(^/metrics|^/apis/v1beta1/healthz)"`,
									},
									Ports: []v1.ContainerPort{
										{
											ContainerPort: constants.OauthProxyPort,
											Name:          "https",
											Protocol:      v1.ProtocolTCP,
										},
									},
									LivenessProbe: &v1.Probe{
										ProbeHandler: v1.ProbeHandler{
											HTTPGet: &v1.HTTPGetAction{
												Path:   "/oauth/healthz",
												Port:   intstr.FromInt(constants.OauthProxyPort),
												Scheme: v1.URISchemeHTTPS,
											},
										},
										InitialDelaySeconds: 30,
										TimeoutSeconds:      1,
										PeriodSeconds:       5,
										SuccessThreshold:    1,
										FailureThreshold:    3,
									},
									ReadinessProbe: &v1.Probe{
										ProbeHandler: v1.ProbeHandler{
											HTTPGet: &v1.HTTPGetAction{
												Path:   "/oauth/healthz",
												Port:   intstr.FromInt(constants.OauthProxyPort),
												Scheme: v1.URISchemeHTTPS,
											},
										},
										InitialDelaySeconds: 5,
										TimeoutSeconds:      1,
										PeriodSeconds:       5,
										SuccessThreshold:    1,
										FailureThreshold:    3,
									},
									Resources: v1.ResourceRequirements{
										Limits: v1.ResourceList{
											v1.ResourceCPU:    resource.MustParse(constants.OauthProxyResourceCPULimit),
											v1.ResourceMemory: resource.MustParse(constants.OauthProxyResourceMemoryLimit),
										},
										Requests: v1.ResourceList{
											v1.ResourceCPU:    resource.MustParse(constants.OauthProxyResourceCPURequest),
											v1.ResourceMemory: resource.MustParse(constants.OauthProxyResourceMemoryRequest),
										},
									},
									VolumeMounts: []v1.VolumeMount{
										{
											Name:      "proxy-tls",
											MountPath: "/etc/tls/private",
										},
									},
									TerminationMessagePath:   "/dev/termination-log",
									TerminationMessagePolicy: "File",
									ImagePullPolicy:          "IfNotPresent",
								},
							},
							Volumes: []v1.Volume{
								{
									Name: "proxy-tls",
									VolumeSource: v1.VolumeSource{
										Secret: &v1.SecretVolumeSource{
											SecretName:  predictorDeploymentKey.Name,
											DefaultMode: func(i int32) *int32 { return &i }(420),
										},
									},
								},
							},
							ServiceAccountName:            constants.KserveServiceAccountName,
							DeprecatedServiceAccount:      constants.KserveServiceAccountName,
							SchedulerName:                 "default-scheduler",
							RestartPolicy:                 "Always",
							TerminationGracePeriodSeconds: &gracePeriod,
							DNSPolicy:                     "ClusterFirst",
							SecurityContext: &v1.PodSecurityContext{
								SELinuxOptions:      nil,
								WindowsOptions:      nil,
								RunAsUser:           nil,
								RunAsGroup:          nil,
								RunAsNonRoot:        nil,
								SupplementalGroups:  nil,
								FSGroup:             nil,
								Sysctls:             nil,
								FSGroupChangePolicy: nil,
								SeccompProfile:      nil,
							},
						},
					},
					Strategy: appsv1.DeploymentStrategy{
						Type: "RollingUpdate",
						RollingUpdate: &appsv1.RollingUpdateDeployment{
							MaxUnavailable: &intstr.IntOrString{Type: 1, IntVal: 0, StrVal: "25%"},
							MaxSurge:       &intstr.IntOrString{Type: 1, IntVal: 0, StrVal: "25%"},
						},
					},
					RevisionHistoryLimit:    &revisionHistory,
					ProgressDeadlineSeconds: &progressDeadlineSeconds,
				},
			}
			Expect(actualDeployment.Spec).To(gomega.Equal(expectedDeployment.Spec))

			//check service
			actualService := &v1.Service{}
			predictorServiceKey := types.NamespacedName{Name: constants.PredictorServiceName(serviceKey.Name),
				Namespace: serviceKey.Namespace}
			Eventually(func() error { return k8sClient.Get(context.TODO(), predictorServiceKey, actualService) }, timeout).
				Should(Succeed())

			expectedService := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      predictorServiceKey.Name,
					Namespace: predictorServiceKey.Namespace,
				},
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{
						{
							Name:       "https",
							Protocol:   "TCP",
							Port:       8443,
							TargetPort: intstr.IntOrString{Type: intstr.String, StrVal: "https"},
						},
					},
					Type:            "ClusterIP",
					SessionAffinity: "None",
					Selector: map[string]string{
						"app": fmt.Sprintf("isvc.%s", constants.PredictorServiceName(serviceName)),
					},
				},
			}
			actualService.Spec.ClusterIP = ""
			actualService.Spec.ClusterIPs = nil
			actualService.Spec.IPFamilies = nil
			actualService.Spec.IPFamilyPolicy = nil
			actualService.Spec.InternalTrafficPolicy = nil
			Expect(actualService.Spec).To(gomega.Equal(expectedService.Spec))

			//check isvc status
			updatedDeployment := actualDeployment.DeepCopy()
			updatedDeployment.Status.Conditions = []appsv1.DeploymentCondition{
				{
					Type:   appsv1.DeploymentAvailable,
					Status: v1.ConditionTrue,
				},
			}
			Expect(k8sClient.Status().Update(context.TODO(), updatedDeployment)).NotTo(gomega.HaveOccurred())

			// verify if InferenceService status is updated
			expectedIsvcStatus := v1beta1.InferenceServiceStatus{
				Status: duckv1.Status{
					Conditions: duckv1.Conditions{
						{
							Type:   v1beta1.IngressReady,
							Status: "True",
						},
						{
							Type:   v1beta1.PredictorReady,
							Status: "True",
						},
						{
							Type:   apis.ConditionReady,
							Status: "True",
						},
					},
				},
				URL: &apis.URL{
					Scheme: "http",
					Host:   "raw-auth-default.example.com",
				},
				Address: &duckv1.Addressable{
					URL: &apis.URL{
						Scheme: "http",
						Host:   fmt.Sprintf("%s-predictor.%s.svc.cluster.local", serviceKey.Name, serviceKey.Namespace),
					},
				},
				Components: map[v1beta1.ComponentType]v1beta1.ComponentStatusSpec{
					v1beta1.PredictorComponent: {
						LatestCreatedRevision: "",
						URL: &apis.URL{
							Scheme: "http",
							Host:   "raw-auth-predictor-default.example.com",
						},
					},
				},
				ModelStatus: v1beta1.ModelStatus{
					TransitionStatus:    "InProgress",
					ModelRevisionStates: &v1beta1.ModelRevisionStates{TargetModelState: "Pending"},
				},
			}
			Eventually(func() string {
				isvc := &v1beta1.InferenceService{}
				if err := k8sClient.Get(context.TODO(), serviceKey, isvc); err != nil {
					return err.Error()
				}
				return cmp.Diff(&expectedIsvcStatus, &isvc.Status, cmpopts.IgnoreTypes(apis.VolatileTime{}))
			}, timeout).Should(gomega.BeEmpty())

		})
	})
})
