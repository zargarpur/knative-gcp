/*
Copyright 2019 Google LLC

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

package pubsub

import (
	"context"
	"errors"
	"fmt"
	"testing"

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
	"github.com/google/knative-gcp/pkg/client/injection/reconciler/events/v1beta1/cloudpubsubsource"
	testingMetadataClient "github.com/google/knative-gcp/pkg/gclient/metadata/testing"
	"github.com/google/knative-gcp/pkg/pubsub/adapter/converters"
	"github.com/google/knative-gcp/pkg/reconciler/identity"
	"github.com/google/knative-gcp/pkg/reconciler/intevents"
	. "github.com/google/knative-gcp/pkg/reconciler/testing"
)

const (
	pubsubName = "my-test-pubsub"
	pubsubUID  = "test-pubsub-uid"
	sinkName   = "sink"

	testNS                                     = "testnamespace"
	testTopicID                                = "test-topic"
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

// Returns an ownerref for the test CloudPubSubSource object
func ownerRef() metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         "events.cloud.google.com/v1beta1",
		Kind:               "CloudPubSubSource",
		Name:               pubsubName,
		UID:                pubsubUID,
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

	table := TableTest{{
		Name: "bad workqueue key",
		// Make sure Reconcile handles bad keys.
		Key: "too/many/parts",
	}, {
		Name: "key not found",
		// Make sure Reconcile handles good keys that don't exist.
		Key: "foo/not-found",
	}, {
		Name: "pullsubscription created",
		Objects: []runtime.Object{
			NewCloudPubSubSource(pubsubName, testNS,
				WithCloudPubSubSourceObjectMetaGeneration(generation),
				WithCloudPubSubSourceTopic(testTopicID),
				WithCloudPubSubSourceSink(sinkGVK, sinkName),
				WithCloudPubSubSourceAnnotations(map[string]string{
					duckv1beta1.ClusterNameAnnotation: testingMetadataClient.FakeClusterName,
				}),
				WithCloudPubSubSourceSetDefaults,
			),
			newSink(),
		},
		Key: testNS + "/" + pubsubName,
		WantStatusUpdates: []clientgotesting.UpdateActionImpl{{
			Object: NewCloudPubSubSource(pubsubName, testNS,
				WithCloudPubSubSourceObjectMetaGeneration(generation),
				WithCloudPubSubSourceStatusObservedGeneration(generation),
				WithCloudPubSubSourceTopic(testTopicID),
				WithCloudPubSubSourceSink(sinkGVK, sinkName),
				WithInitCloudPubSubSourceConditions,
				WithCloudPubSubSourceObjectMetaGeneration(generation),
				WithCloudPubSubSourceAnnotations(map[string]string{
					duckv1beta1.ClusterNameAnnotation: testingMetadataClient.FakeClusterName,
				}),
				WithCloudPubSubSourceSetDefaults,
				WithCloudPubSubSourcePullSubscriptionUnknown("PullSubscriptionNotConfigured", "PullSubscription has not yet been reconciled"),
			),
		}},
		WantCreates: []runtime.Object{
			NewPullSubscription(pubsubName, testNS,
				WithPullSubscriptionSpec(inteventsv1beta1.PullSubscriptionSpec{
					Topic: testTopicID,
					PubSubSpec: duckv1beta1.PubSubSpec{
						Secret: &secret,
					},
					AdapterType: string(converters.CloudPubSub),
				}),
				WithPullSubscriptionSink(sinkGVK, sinkName),
				WithPullSubscriptionLabels(map[string]string{
					"receive-adapter":                     receiveAdapterName,
					"events.cloud.google.com/source-name": pubsubName,
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
			patchFinalizers(testNS, pubsubName, true),
		},
		WantEvents: []string{
			Eventf(corev1.EventTypeNormal, "FinalizerUpdate", "Updated %q finalizers", pubsubName),
			Eventf(corev1.EventTypeWarning, intevents.PullSubscriptionStatusPropagateFailedReason, "%s: PullSubscription %q has not yet been reconciled", failedToPropagatePullSubscriptionStatusMsg, pubsubName),
		},
	}, {
		Name: "pullsubscription exists and the status is false",
		Objects: []runtime.Object{
			NewCloudPubSubSource(pubsubName, testNS,
				WithCloudPubSubSourceObjectMetaGeneration(generation),
				WithCloudPubSubSourceTopic(testTopicID),
				WithCloudPubSubSourceSink(sinkGVK, sinkName),
				WithCloudPubSubSourceSetDefaults,
			),
			NewPullSubscription(pubsubName, testNS,
				WithPullSubscriptionSpec(inteventsv1beta1.PullSubscriptionSpec{
					Topic: testTopicID,
					PubSubSpec: duckv1beta1.PubSubSpec{
						Secret: &secret,
						SourceSpec: duckv1.SourceSpec{
							Sink: newSinkDestination(),
						},
					},
					AdapterType: string(converters.CloudPubSub),
				}),
				WithPullSubscriptionReadyStatus(corev1.ConditionFalse, "PullSubscriptionFalse", "status false test message")),
			newSink(),
		},
		Key: testNS + "/" + pubsubName,
		WantStatusUpdates: []clientgotesting.UpdateActionImpl{{
			Object: NewCloudPubSubSource(pubsubName, testNS,
				WithCloudPubSubSourceObjectMetaGeneration(generation),
				WithCloudPubSubSourceStatusObservedGeneration(generation),
				WithCloudPubSubSourceTopic(testTopicID),
				WithCloudPubSubSourceSink(sinkGVK, sinkName),
				WithInitCloudPubSubSourceConditions,
				WithCloudPubSubSourceObjectMetaGeneration(generation),
				WithCloudPubSubSourcePullSubscriptionFailed("PullSubscriptionFalse", "status false test message"),
				WithCloudPubSubSourceSetDefaults,
			),
		}},
		WantPatches: []clientgotesting.PatchActionImpl{
			patchFinalizers(testNS, pubsubName, true),
		},
		WantEvents: []string{
			Eventf(corev1.EventTypeNormal, "FinalizerUpdate", "Updated %q finalizers", pubsubName),
			Eventf(corev1.EventTypeWarning, intevents.PullSubscriptionStatusPropagateFailedReason, "%s: the status of PullSubscription %q is False", failedToPropagatePullSubscriptionStatusMsg, pubsubName),
		},
	}, {
		Name: "pullsubscription exists and the status is unknown",
		Objects: []runtime.Object{
			NewCloudPubSubSource(pubsubName, testNS,
				WithCloudPubSubSourceObjectMetaGeneration(generation),
				WithCloudPubSubSourceTopic(testTopicID),
				WithCloudPubSubSourceSink(sinkGVK, sinkName),
				WithCloudPubSubSourceSetDefaults,
			),
			NewPullSubscription(pubsubName, testNS,
				WithPullSubscriptionSpec(inteventsv1beta1.PullSubscriptionSpec{
					Topic: testTopicID,
					PubSubSpec: duckv1beta1.PubSubSpec{
						Secret: &secret,
						SourceSpec: duckv1.SourceSpec{
							Sink: newSinkDestination(),
						},
					},
					AdapterType: string(converters.CloudPubSub),
				}),
				WithPullSubscriptionReadyStatus(corev1.ConditionUnknown, "PullSubscriptionUnknown", "status unknown test message")),
			newSink(),
		},
		Key: testNS + "/" + pubsubName,
		WantStatusUpdates: []clientgotesting.UpdateActionImpl{{
			Object: NewCloudPubSubSource(pubsubName, testNS,
				WithCloudPubSubSourceObjectMetaGeneration(generation),
				WithCloudPubSubSourceStatusObservedGeneration(generation),
				WithCloudPubSubSourceTopic(testTopicID),
				WithCloudPubSubSourceSink(sinkGVK, sinkName),
				WithInitCloudPubSubSourceConditions,
				WithCloudPubSubSourceObjectMetaGeneration(generation),
				WithCloudPubSubSourcePullSubscriptionUnknown("PullSubscriptionUnknown", "status unknown test message"),
				WithCloudPubSubSourceSetDefaults,
			),
		}},
		WantPatches: []clientgotesting.PatchActionImpl{
			patchFinalizers(testNS, pubsubName, true),
		},
		WantEvents: []string{
			Eventf(corev1.EventTypeNormal, "FinalizerUpdate", "Updated %q finalizers", pubsubName),
			Eventf(corev1.EventTypeWarning, intevents.PullSubscriptionStatusPropagateFailedReason, "%s: the status of PullSubscription %q is Unknown", failedToPropagatePullSubscriptionStatusMsg, pubsubName),
		},
	}, {
		Name: "pullsubscription exists and ready, with retry",
		Objects: []runtime.Object{
			NewCloudPubSubSource(pubsubName, testNS,
				WithCloudPubSubSourceObjectMetaGeneration(generation),
				WithCloudPubSubSourceTopic(testTopicID),
				WithCloudPubSubSourceSink(sinkGVK, sinkName),
				WithCloudPubSubSourceSetDefaults,
			),
			NewPullSubscription(pubsubName, testNS,
				WithPullSubscriptionSpec(inteventsv1beta1.PullSubscriptionSpec{
					Topic: testTopicID,
					PubSubSpec: duckv1beta1.PubSubSpec{
						Secret: &secret,
						SourceSpec: duckv1.SourceSpec{
							Sink: newSinkDestination(),
						},
					},
					AdapterType: string(converters.CloudPubSub),
				}),
				WithPullSubscriptionReady(sinkURI),
				WithPullSubscriptionReadyStatus(corev1.ConditionTrue, "PullSubscriptionNoReady", ""),
			),
			newSink(),
		},
		Key: testNS + "/" + pubsubName,
		WithReactors: []clientgotesting.ReactionFunc{
			func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
				if attempts != 0 || !action.Matches("update", "cloudpubsubsources") {
					return false, nil, nil
				}
				attempts++
				return true, nil, apierrs.NewConflict(v1beta1.Resource("foo"), "bar", errors.New("foo"))
			},
		},
		WantStatusUpdates: []clientgotesting.UpdateActionImpl{{
			Object: NewCloudPubSubSource(pubsubName, testNS,
				WithCloudPubSubSourceObjectMetaGeneration(generation),
				WithCloudPubSubSourceStatusObservedGeneration(generation),
				WithCloudPubSubSourceTopic(testTopicID),
				WithCloudPubSubSourceSink(sinkGVK, sinkName),
				WithInitCloudPubSubSourceConditions,
				WithCloudPubSubSourcePullSubscriptionReady,
				WithCloudPubSubSourceSinkURI(pubsubSinkURL),
				WithCloudPubSubSourceSubscriptionID(SubscriptionID),
				WithCloudPubSubSourceSetDefaults,
			),
		}, {
			Object: NewCloudPubSubSource(pubsubName, testNS,
				WithCloudPubSubSourceObjectMetaGeneration(generation),
				WithCloudPubSubSourceStatusObservedGeneration(generation),
				WithCloudPubSubSourceTopic(testTopicID),
				WithCloudPubSubSourceSink(sinkGVK, sinkName),
				WithInitCloudPubSubSourceConditions,
				WithCloudPubSubSourcePullSubscriptionReady,
				WithCloudPubSubSourceSinkURI(pubsubSinkURL),
				WithCloudPubSubSourceSubscriptionID(SubscriptionID),
				WithCloudPubSubSourceFinalizers("cloudpubsubsources.events.cloud.google.com"),
				WithCloudPubSubSourceSetDefaults,
			),
		}},
		WantPatches: []clientgotesting.PatchActionImpl{
			patchFinalizers(testNS, pubsubName, true),
		},
		WantEvents: []string{
			Eventf(corev1.EventTypeNormal, "FinalizerUpdate", "Updated %q finalizers", pubsubName),
			Eventf(corev1.EventTypeNormal, reconciledSuccessReason, `CloudPubSubSource reconciled: "%s/%s"`, testNS, pubsubName),
		},
	}}

	defer logtesting.ClearAll()
	table.Test(t, MakeFactory(func(ctx context.Context, listers *Listers, cmw configmap.Watcher, _ map[string]interface{}) controller.Reconciler {
		r := &Reconciler{
			PubSubBase: intevents.NewPubSubBase(ctx,
				&intevents.PubSubBaseArgs{
					ControllerAgentName: controllerAgentName,
					ReceiveAdapterName:  receiveAdapterName,
					ReceiveAdapterType:  string(converters.CloudPubSub),
					ConfigWatcher:       cmw,
				}),
			Identity:     identity.NewIdentity(ctx, NoopIAMPolicyManager, NewGCPAuthTestStore(t, nil)),
			pubsubLister: listers.GetCloudPubSubSourceLister(),
		}
		return cloudpubsubsource.NewReconciler(ctx, r.Logger, r.RunClientSet, listers.GetCloudPubSubSourceLister(), r.Recorder, r)
	}))

}
