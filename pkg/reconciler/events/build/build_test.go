/*
Copyright 2020 Google LLC

Licensed under the Apache License, Veroute.on 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package build

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/knative-gcp/pkg/apis/events"
	"github.com/google/knative-gcp/pkg/pubsub/adapter/converters"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	clientgotesting "k8s.io/client-go/testing"
	duckv1 "knative.dev/pkg/apis/duck/v1"

	"knative.dev/pkg/apis"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	logtesting "knative.dev/pkg/logging/testing"

	. "knative.dev/pkg/reconciler/testing"

	duckv1beta1 "github.com/google/knative-gcp/pkg/apis/duck/v1beta1"
	"github.com/google/knative-gcp/pkg/apis/events/v1beta1"
	inteventsv1beta1 "github.com/google/knative-gcp/pkg/apis/intevents/v1beta1"
	"github.com/google/knative-gcp/pkg/client/injection/reconciler/events/v1beta1/cloudbuildsource"
	testingMetadataClient "github.com/google/knative-gcp/pkg/gclient/metadata/testing"
	"github.com/google/knative-gcp/pkg/reconciler/identity"
	"github.com/google/knative-gcp/pkg/reconciler/intevents"
	. "github.com/google/knative-gcp/pkg/reconciler/testing"
)

const (
	buildName = "my-test-build"
	buildUID  = "test-build-uid"
	sinkName  = "sink"

	testNS                                     = "testnamespace"
	testTopicID                                = events.CloudBuildTopic
	generation                                 = 1
	failedToPropagatePullSubscriptionStatusMsg = `Failed to propagate PullSubscription status`
)

var (
	trueVal  = true
	falseVal = false

	sinkDNS = sinkName + ".mynamespace.svc.cluster.local"
	sinkURI = apis.HTTP(sinkDNS)

	sinkGVK = metav1.GroupVersionKind{
		Group:   "testing.cloud.google.com",
		Version: "v1beta1",
		Kind:    "Sink",
	}

	secret = corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{
			Name: "google-cloud-key",
		},
		Key: "key.json",
	}

	gServiceAccount = "test123@test123.iam.gserviceaccount.com"
)

func init() {
	// Add types to scheme
	_ = v1beta1.AddToScheme(scheme.Scheme)
}

// Returns an ownerref for the test CloudBuildSource object
func ownerRef() metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         "events.cloud.google.com/v1beta1",
		Kind:               "CloudBuildSource",
		Name:               buildName,
		UID:                buildUID,
		Controller:         &trueVal,
		BlockOwnerDeletion: &trueVal,
	}
}

func patchFinalizers(namespace, name string, add bool) clientgotesting.PatchActionImpl {
	action := clientgotesting.PatchActionImpl{}
	action.Name = name
	action.Namespace = namespace
	var fname string
	if add {
		fname = fmt.Sprintf("%q", resourceGroup)
	}
	patch := `{"metadata":{"finalizers":[` + fname + `],"resourceVersion":""}}`
	action.Patch = []byte(patch)
	return action
}

func newSink() *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "testing.cloud.google.com/v1beta1",
			"kind":       "Sink",
			"metadata": map[string]interface{}{
				"namespace": testNS,
				"name":      sinkName,
			},
			"status": map[string]interface{}{
				"address": map[string]interface{}{
					"hostname": sinkDNS,
				},
			},
		},
	}
}

func newSinkDestination() duckv1.Destination {
	return duckv1.Destination{
		Ref: &duckv1.KReference{
			APIVersion: "testing.cloud.google.com/v1beta1",
			Kind:       "Sink",
			Namespace:  testNS,
			Name:       sinkName,
		},
	}
}

// TODO add a unit test for successfully creating a k8s service account, after issue https://github.com/google/knative-gcp/issues/657 gets solved.
func TestAllCases(t *testing.T) {
	attempts := 0
	pubsubSinkURL := sinkURI

	table := TableTest{
		{
			Name: "bad workqueue key",
			// Make sure Reconcile handles bad keys.
			Key: "too/many/parts",
		}, {
			Name: "key not found",
			// Make sure Reconcile handles good keys that don't exist.
			Key: "foo/not-found",
		},
		{
			Name: "pullsubscription created",
			Objects: []runtime.Object{
				NewCloudBuildSource(buildName, testNS,
					WithCloudBuildSourceObjectMetaGeneration(generation),
					WithCloudBuildSourceSink(sinkGVK, sinkName),
					WithCloudBuildSourceAnnotations(map[string]string{
						duckv1beta1.ClusterNameAnnotation: testingMetadataClient.FakeClusterName,
					}),
					WithCloudBuildSourceSetDefault,
				),
				newSink(),
			},
			Key: testNS + "/" + buildName,
			WantStatusUpdates: []clientgotesting.UpdateActionImpl{{
				Object: NewCloudBuildSource(buildName, testNS,
					WithCloudBuildSourceObjectMetaGeneration(generation),
					WithCloudBuildSourceStatusObservedGeneration(generation),
					WithCloudBuildSourceSink(sinkGVK, sinkName),
					WithInitCloudBuildSourceConditions,
					WithCloudBuildSourceObjectMetaGeneration(generation),
					WithCloudBuildSourceAnnotations(map[string]string{
						duckv1beta1.ClusterNameAnnotation: testingMetadataClient.FakeClusterName,
					}),
					WithCloudBuildSourceSetDefault,
					WithCloudBuildSourcePullSubscriptionUnknown("PullSubscriptionNotConfigured", "PullSubscription has not yet been reconciled"),
				),
			}},
			WantCreates: []runtime.Object{
				NewPullSubscription(buildName, testNS,
					WithPullSubscriptionSpec(inteventsv1beta1.PullSubscriptionSpec{
						Topic: testTopicID,
						PubSubSpec: duckv1beta1.PubSubSpec{
							Secret: &secret,
							SourceSpec: duckv1.SourceSpec{
								Sink: newSinkDestination(),
							},
						},
						AdapterType: string(converters.CloudBuild),
					}),
					WithPullSubscriptionSink(sinkGVK, sinkName),
					WithPullSubscriptionLabels(map[string]string{
						"receive-adapter":                     receiveAdapterName,
						"events.cloud.google.com/source-name": buildName,
					}),
					WithPullSubscriptionAnnotations(map[string]string{
						"metrics-resource-group":          resourceGroup,
						duckv1beta1.ClusterNameAnnotation: testingMetadataClient.FakeClusterName,
					}),
					WithPullSubscriptionOwnerReferences([]metav1.OwnerReference{ownerRef()}),
					WithPullSubscriptionDefaultGCPAuth,
				),
			},
			WantPatches: []clientgotesting.PatchActionImpl{
				patchFinalizers(testNS, buildName, true),
			},
			WantEvents: []string{
				Eventf(corev1.EventTypeNormal, "FinalizerUpdate", "Updated %q finalizers", buildName),
				Eventf(corev1.EventTypeWarning, intevents.PullSubscriptionStatusPropagateFailedReason, "%s: PullSubscription %q has not yet been reconciled", failedToPropagatePullSubscriptionStatusMsg, buildName),
			},
		}, {
			Name: "pullsubscription exists and the status is false",
			Objects: []runtime.Object{
				NewCloudBuildSource(buildName, testNS,
					WithCloudBuildSourceObjectMetaGeneration(generation),
					WithCloudBuildSourceSink(sinkGVK, sinkName),
					WithCloudBuildSourceSetDefault,
				),
				NewPullSubscription(buildName, testNS,
					WithPullSubscriptionSpec(inteventsv1beta1.PullSubscriptionSpec{
						Topic: testTopicID,
						PubSubSpec: duckv1beta1.PubSubSpec{
							Secret: &secret,
							SourceSpec: duckv1.SourceSpec{
								Sink: newSinkDestination(),
							},
						},
						AdapterType: string(converters.CloudBuild),
					}),
					WithPullSubscriptionReadyStatus(corev1.ConditionFalse, "PullSubscriptionFalse", "status false test message")),
				newSink(),
			},
			Key: testNS + "/" + buildName,
			WantStatusUpdates: []clientgotesting.UpdateActionImpl{{
				Object: NewCloudBuildSource(buildName, testNS,
					WithCloudBuildSourceObjectMetaGeneration(generation),
					WithCloudBuildSourceStatusObservedGeneration(generation),
					WithCloudBuildSourceSink(sinkGVK, sinkName),
					WithInitCloudBuildSourceConditions,
					WithCloudBuildSourceObjectMetaGeneration(generation),
					WithCloudBuildSourcePullSubscriptionFailed("PullSubscriptionFalse", "status false test message"),
					WithCloudBuildSourceSetDefault,
				),
			}},
			WantPatches: []clientgotesting.PatchActionImpl{
				patchFinalizers(testNS, buildName, true),
			},
			WantEvents: []string{
				Eventf(corev1.EventTypeNormal, "FinalizerUpdate", "Updated %q finalizers", buildName),
				Eventf(corev1.EventTypeWarning, intevents.PullSubscriptionStatusPropagateFailedReason, "%s: the status of PullSubscription %q is False", failedToPropagatePullSubscriptionStatusMsg, buildName),
			},
		}, {
			Name: "pullsubscription exists and the status is unknown",
			Objects: []runtime.Object{
				NewCloudBuildSource(buildName, testNS,
					WithCloudBuildSourceObjectMetaGeneration(generation),
					WithCloudBuildSourceSink(sinkGVK, sinkName),
					WithCloudBuildSourceSetDefault,
				),
				NewPullSubscription(buildName, testNS,
					WithPullSubscriptionSpec(inteventsv1beta1.PullSubscriptionSpec{
						Topic: testTopicID,
						PubSubSpec: duckv1beta1.PubSubSpec{
							Secret: &secret,
							SourceSpec: duckv1.SourceSpec{
								Sink: newSinkDestination(),
							},
						},
						AdapterType: string(converters.CloudBuild),
					}),
					WithPullSubscriptionReadyStatus(corev1.ConditionUnknown, "PullSubscriptionUnknown", "status unknown test message")),
				newSink(),
			},
			Key: testNS + "/" + buildName,
			WantStatusUpdates: []clientgotesting.UpdateActionImpl{{
				Object: NewCloudBuildSource(buildName, testNS,
					WithCloudBuildSourceObjectMetaGeneration(generation),
					WithCloudBuildSourceStatusObservedGeneration(generation),
					WithCloudBuildSourceSink(sinkGVK, sinkName),
					WithInitCloudBuildSourceConditions,
					WithCloudBuildSourceObjectMetaGeneration(generation),
					WithCloudBuildSourcePullSubscriptionUnknown("PullSubscriptionUnknown", "status unknown test message"),
					WithCloudBuildSourceSetDefault,
				),
			}},
			WantPatches: []clientgotesting.PatchActionImpl{
				patchFinalizers(testNS, buildName, true),
			},
			WantEvents: []string{
				Eventf(corev1.EventTypeNormal, "FinalizerUpdate", "Updated %q finalizers", buildName),
				Eventf(corev1.EventTypeWarning, intevents.PullSubscriptionStatusPropagateFailedReason, "%s: the status of PullSubscription %q is Unknown", failedToPropagatePullSubscriptionStatusMsg, buildName),
			},
		}, {
			Name: "pullsubscription exists and ready, with retry",
			Objects: []runtime.Object{
				NewCloudBuildSource(buildName, testNS,
					WithCloudBuildSourceObjectMetaGeneration(generation),
					WithCloudBuildSourceSink(sinkGVK, sinkName),
					WithCloudBuildSourceSetDefault,
				),
				NewPullSubscription(buildName, testNS,
					WithPullSubscriptionSpec(inteventsv1beta1.PullSubscriptionSpec{
						Topic: testTopicID,
						PubSubSpec: duckv1beta1.PubSubSpec{
							Secret: &secret,
							SourceSpec: duckv1.SourceSpec{
								Sink: newSinkDestination(),
							},
						},
						AdapterType: string(converters.CloudBuild),
					}),
					WithPullSubscriptionReady(sinkURI),
					WithPullSubscriptionReadyStatus(corev1.ConditionTrue, "PullSubscriptionNoReady", ""),
				),
				newSink(),
			},
			Key: testNS + "/" + buildName,
			WithReactors: []clientgotesting.ReactionFunc{
				func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
					if attempts != 0 || !action.Matches("update", "cloudbuildsources") {
						return false, nil, nil
					}
					attempts++
					return true, nil, apierrs.NewConflict(v1beta1.Resource("foo"), "bar", errors.New("foo"))
				},
			},
			WantStatusUpdates: []clientgotesting.UpdateActionImpl{{
				Object: NewCloudBuildSource(buildName, testNS,
					WithCloudBuildSourceObjectMetaGeneration(generation),
					WithCloudBuildSourceStatusObservedGeneration(generation),
					WithCloudBuildSourceSink(sinkGVK, sinkName),
					WithInitCloudBuildSourceConditions,
					WithCloudBuildSourcePullSubscriptionReady(),
					WithCloudBuildSourceSinkURI(pubsubSinkURL),
					WithCloudBuildSourceSubscriptionID(SubscriptionID),
					WithCloudBuildSourceSetDefault,
				),
			}, {
				Object: NewCloudBuildSource(buildName, testNS,
					WithCloudBuildSourceObjectMetaGeneration(generation),
					WithCloudBuildSourceStatusObservedGeneration(generation),
					WithCloudBuildSourceSink(sinkGVK, sinkName),
					WithInitCloudBuildSourceConditions,
					WithCloudBuildSourcePullSubscriptionReady(),
					WithCloudBuildSourceSinkURI(pubsubSinkURL),
					WithCloudBuildSourceSubscriptionID(SubscriptionID),
					WithCloudBuildSourceFinalizers("cloudbuildsources.events.cloud.google.com"),
					WithCloudBuildSourceSetDefault,
				),
			}},
			WantPatches: []clientgotesting.PatchActionImpl{
				patchFinalizers(testNS, buildName, true),
			},
			WantEvents: []string{
				Eventf(corev1.EventTypeNormal, "FinalizerUpdate", "Updated %q finalizers", buildName),
				Eventf(corev1.EventTypeNormal, reconciledSuccessReason, `CloudBuildSource reconciled: "%s/%s"`, testNS, buildName),
			},
		}}

	defer logtesting.ClearAll()
	table.Test(t, MakeFactory(func(ctx context.Context, listers *Listers, cmw configmap.Watcher, _ map[string]interface{}) controller.Reconciler {
		r := &Reconciler{
			PubSubBase: intevents.NewPubSubBase(ctx,
				&intevents.PubSubBaseArgs{
					ControllerAgentName: controllerAgentName,
					ReceiveAdapterName:  receiveAdapterName,
					ReceiveAdapterType:  string(converters.CloudBuild),
					ConfigWatcher:       cmw,
				}),
			Identity:             identity.NewIdentity(ctx, NoopIAMPolicyManager, NewGCPAuthTestStore(t, nil)),
			buildLister:          listers.GetCloudBuildSourceLister(),
			serviceAccountLister: listers.GetServiceAccountLister(),
		}
		return cloudbuildsource.NewReconciler(ctx, r.Logger, r.RunClientSet, listers.GetCloudBuildSourceLister(), r.Recorder, r)
	}))

}
