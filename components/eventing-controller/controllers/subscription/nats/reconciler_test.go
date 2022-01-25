package nats

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kyma-project/kyma/components/eventing-controller/controllers/events"

	"github.com/kyma-project/kyma/components/eventing-controller/pkg/handlers"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/envtest/printer"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	kymalogger "github.com/kyma-project/kyma/common/logging/logger"
	eventingv1alpha1 "github.com/kyma-project/kyma/components/eventing-controller/api/v1alpha1"
	"github.com/kyma-project/kyma/components/eventing-controller/logger"
	"github.com/kyma-project/kyma/components/eventing-controller/pkg/application/applicationtest"
	"github.com/kyma-project/kyma/components/eventing-controller/pkg/application/fake"
	"github.com/kyma-project/kyma/components/eventing-controller/pkg/env"
	reconcilertesting "github.com/kyma-project/kyma/components/eventing-controller/testing"
)

const (
	natsPort = 4221

	smallTimeout         = 10 * time.Second
	smallPollingInterval = 1 * time.Second

	timeout         = 60 * time.Second
	pollingInterval = 5 * time.Second

	namespaceName          = "test"
	subscriptionNameFormat = "nats-sub-%d"
	subscriberNameFormat   = "subscriber-%d"
)

var _ = Describe("NATS reconciler tests", func() {
	var id int

	AfterEach(func() {
		id++
	})

	testCases := []struct{ context, eventTypePrefix, natsSubjectToPublish, eventTypeToSubscribe string }{
		{
			context:              "with non-empty eventTypePrefix",
			eventTypePrefix:      reconcilertesting.EventTypePrefix,
			natsSubjectToPublish: reconcilertesting.OrderCreatedEventType,
			eventTypeToSubscribe: reconcilertesting.OrderCreatedEventTypeNotClean,
		},
		{
			context:              "with empty eventTypePrefix",
			eventTypePrefix:      reconcilertesting.EventTypePrefixEmpty,
			natsSubjectToPublish: reconcilertesting.OrderCreatedEventTypePrefixEmpty,
			eventTypeToSubscribe: reconcilertesting.OrderCreatedEventTypeNotCleanPrefixEmpty,
		},
	}
	for _, tc := range testCases {

		When("NATS server is not reachable and max retries are exceeded", func() {
			It("Should mark the Subscription as not ready until NATS is reachable again", func() {
				subscriptionName := fmt.Sprintf(subscriptionNameFormat, id) + "-valid"
				subscriberName := fmt.Sprintf(subscriberNameFormat, id) + "-valid"
				sink := reconcilertesting.GetValidSink(namespaceName, subscriberName)

				ctx := context.Background()
				natsPort := natsPort + id // todo the id increasement is placed now in the "afterEach", so it gets executed after each "It"
				natsServer, natsURL := startNATS(natsPort)
				defer reconcilertesting.ShutDownNATSServer(natsServer)
				cancel = startReconciler(tc.eventTypePrefix, defaultSinkValidator, natsURL)
				defer cancel()

				// create subscriber svc
				subscriberSvc := reconcilertesting.NewSubscriberSvc(subscriberName, namespaceName)
				byEnsuringSubscriberSvcCreated(ctx, subscriberSvc)

				// create subscription
				subscription := reconcilertesting.NewSubscription(
					subscriptionName,
					namespaceName,
					reconcilertesting.WithSpecificEventTypeFilter("", tc.eventTypeToSubscribe))
				subscription.Spec.Sink = sink
				byEnsuringSubscriptionCreated(ctx, subscription)

				getSubscription(ctx, subscription).Should(And(
					reconcilertesting.HaveSubscriptionName(subscriptionName),
					reconcilertesting.HaveCondition(eventingv1alpha1.MakeCondition(
						eventingv1alpha1.ConditionSubscriptionActive,
						eventingv1alpha1.ConditionReasonNATSSubscriptionActive,
						v1.ConditionTrue, "")),
				))

				natsServer.Shutdown()
				getSubscription(ctx, subscription, timeout, pollingInterval).Should(And(
					reconcilertesting.HaveSubscriptionName(subscriptionName),
					reconcilertesting.HaveSubscriptionNotReady()),
				)

				_, _ = startNATS(natsPort)
				getSubscription(ctx, subscription, timeout, pollingInterval).Should(And(
					reconcilertesting.HaveSubscriptionName(subscriptionName),
					reconcilertesting.HaveSubscriptionReady()),
				)
			})
		})

		When("Updating the clean event types in the Subscription status", func() {
			//  set default expectations
			defaultCondition := eventingv1alpha1.MakeCondition(
				eventingv1alpha1.ConditionSubscriptionActive,
				eventingv1alpha1.ConditionReasonNATSSubscriptionActive,
				v1.ConditionTrue, "")
			defaultConfiguration := &eventingv1alpha1.SubscriptionConfig{
				MaxInFlightMessages: defaultSubsConfig.MaxInFlightMessages}

			// start reconciler
			cancel = startReconciler(tc.eventTypePrefix, defaultSinkValidator, natsURL)
			defer cancel()

			// create a subscriber service
			subscriberName := fmt.Sprintf(subscriberNameFormat, testID)
			subscriberSvc := reconcilertesting.NewSubscriberSvc(subscriberName, namespaceName)
			ctx := context.Background()
			It("should have created a subscriber service", func() {
				byEnsuringSubscriberSvcCreated(ctx, subscriberSvc)
			})

			// create a Subscription
			subscriptionName := fmt.Sprintf(subscriptionNameFormat, testID)
			subscription := reconcilertesting.NewSubscription(subscriptionName, namespaceName,
				reconcilertesting.WithEmptyFilter,
				reconcilertesting.WithWebhookForNats,
				reconcilertesting.WithValidSink(namespaceName, subscriberSvc.Name),
			)

			// setting up checking functions
			actualSubscription := func() AsyncAssertion { return getSubscription(ctx, subscription) }
			byCheckingAgainstDefaults := func() {
				By("checking, it holds the correct name")
				actualSubscription().Should(reconcilertesting.HaveSubscriptionName(subscriptionName))

				By("checking against the default condition")
				actualSubscription().Should(reconcilertesting.HaveCondition(defaultCondition))

				By("checking against the default configuration")
				actualSubscription().Should(reconcilertesting.HaveSubsConfiguration(defaultConfiguration))
			}

			Context("creating a Subscription without filters", func() {
				It("should been created successfully without clean event types", func() {
					byEnsuringSubscriptionCreated(ctx, subscription)

					byCheckingAgainstDefaults()

					By("checking, it has no clean event types")
					actualSubscription().Should(reconcilertesting.HaveCleanEventTypes(nil))
				})
			})

			Context("adding filters to a Subscription without filters", func() {
				// setting the clean event types the subscription is expected to contain
				expectedCleanEventTypes := []string{
					fmt.Sprintf("%s0", tc.natsSubjectToPublish),
					fmt.Sprintf("%s1", tc.natsSubjectToPublish),
				}

				// adding event type filters to the subscription
				eventTypesFilters := []string{
					fmt.Sprintf("%s0", tc.eventTypeToSubscribe),
					fmt.Sprintf("%s1", tc.eventTypeToSubscribe),
				}
				for _, f := range eventTypesFilters {
					reconcilertesting.AddSpecificEventTypeFilter(reconcilertesting.EventSource, f, subscription)
				}

				//test
				It("should add clean event types to the Subscription after adding a filter", func() {
					byEnsuringSubscriptionUpdated(ctx, subscription)

					byCheckingAgainstDefaults()

					By("checking, the Subscription contains the expected clean event types")
					actualSubscription().Should(reconcilertesting.HaveCleanEventTypes(expectedCleanEventTypes))
				})
			})

			Context("changing the existing filters of a Subscription", func() {
				// setting the clean event types the subscription is expected to contain
				expectedCleanEventTypes := []string{
					fmt.Sprintf("%s0alpha", tc.natsSubjectToPublish),
					fmt.Sprintf("%s1alpha", tc.natsSubjectToPublish),
				}

				// changing the existing filters
				for _, f := range subscription.Spec.Filter.Filters {
					f.EventType.Value = fmt.Sprintf("%salpha", f.EventType.Value)
				}

				// test
				It("should have also update the clean event types", func() {
					byEnsuringSubscriptionUpdated(ctx, subscription)

					byCheckingAgainstDefaults()

					By("should have changed the clean event types according the modified filters")
					getSubscription(ctx, subscription).Should(reconcilertesting.HaveCleanEventTypes(expectedCleanEventTypes))
				})
			})

			Context("deleting one of two filters of a Subscription", func() {
				// setting the clean event types the subscription is expected to contain
				expectedCleanEventTypes := []string{
					fmt.Sprintf("%s0alpha", tc.natsSubjectToPublish),
				}

				// remove one of the two filters
				subscription.Spec.Filter.Filters = subscription.Spec.Filter.Filters[:1]

				// test
				It("should also remove one of the clean event types", func() {
					byEnsuringSubscriptionUpdated(ctx, subscription)

					byCheckingAgainstDefaults()

					By("checking, that the corresponding clean event type was removed as well")
					getSubscription(ctx, subscription).Should(reconcilertesting.HaveCleanEventTypes(expectedCleanEventTypes))
				})
			})
		})

		When("Create/Delete Subscription", func() {
			It("Should create/delete NATS Subscription", func() {
				ctx := context.Background()
				cancel = startReconciler(tc.eventTypePrefix, defaultSinkValidator, natsURL)
				defer cancel()
				subscriptionName := fmt.Sprintf(subscriptionNameFormat, id)
				subscriberName := fmt.Sprintf(subscriberNameFormat, id)

				// create subscriber svc
				subscriberSvc := reconcilertesting.NewSubscriberSvc(subscriberName, namespaceName)
				byEnsuringSubscriberSvcCreated(ctx, subscriberSvc)

				// create subscription
				optFilter := reconcilertesting.WithSpecificEventTypeFilter(reconcilertesting.EventSource, tc.eventTypeToSubscribe)
				optWebhook := reconcilertesting.WithWebhookForNats
				subscription := reconcilertesting.NewSubscription(subscriptionName, namespaceName, optFilter, optWebhook)
				reconcilertesting.SetValidSink(namespaceName, subscriberSvc.Name, subscription)
				byEnsuringSubscriptionCreated(ctx, subscription)

				getSubscription(ctx, subscription).Should(And(
					reconcilertesting.HaveSubscriptionName(subscriptionName),
					reconcilertesting.HaveCondition(eventingv1alpha1.MakeCondition(
						eventingv1alpha1.ConditionSubscriptionActive,
						eventingv1alpha1.ConditionReasonNATSSubscriptionActive,
						v1.ConditionTrue, "")),
					reconcilertesting.HaveSubsConfiguration(&eventingv1alpha1.SubscriptionConfig{
						MaxInFlightMessages: defaultSubsConfig.MaxInFlightMessages,
					}),
				))

				// check for subscription at nats
				backendSubscription := getSubscriptionFromNats(natsBackend.GetAllSubscriptions(), subscriptionName)
				Expect(backendSubscription).NotTo(BeNil())
				Expect(backendSubscription.IsValid()).To(BeTrue())
				Expect(backendSubscription.Subject).Should(Equal(tc.natsSubjectToPublish))

				Expect(k8sClient.Delete(ctx, subscription)).Should(BeNil())
				isSubscriptionDeleted(ctx, subscription).Should(reconcilertesting.HaveNotFoundSubscription(true))
			})
		})

		When("Subscription has valid sink", func() {
			subscriptionName := fmt.Sprintf(subscriptionNameFormat, id) + "-valid"
			subscriberName := fmt.Sprintf(subscriberNameFormat, id) + "-valid"
			sink := reconcilertesting.GetValidSink(namespaceName, subscriberName)
			testCreatingSubscription := func(sink string) {
				ctx := context.Background()
				cancel = startReconciler(tc.eventTypePrefix, defaultSinkValidator, natsURL)
				defer cancel()

				// create subscriber svc
				subscriberSvc := reconcilertesting.NewSubscriberSvc(subscriberName, namespaceName)
				byEnsuringSubscriberSvcCreated(ctx, subscriberSvc)

				// create subscription
				subscription := reconcilertesting.NewSubscription(subscriptionName, namespaceName, reconcilertesting.WithSpecificEventTypeFilter("", tc.eventTypeToSubscribe))
				subscription.Spec.Sink = sink
				byEnsuringSubscriptionCreated(ctx, subscription)

				getSubscription(ctx, subscription).Should(And(
					reconcilertesting.HaveSubscriptionName(subscriptionName),
					reconcilertesting.HaveCondition(eventingv1alpha1.MakeCondition(
						eventingv1alpha1.ConditionSubscriptionActive,
						eventingv1alpha1.ConditionReasonNATSSubscriptionActive,
						v1.ConditionTrue, "")),
				))

				Expect(k8sClient.Delete(ctx, subscription)).Should(BeNil())
				isSubscriptionDeleted(ctx, subscription).Should(reconcilertesting.HaveNotFoundSubscription(true))

				Expect(k8sClient.Delete(ctx, subscriberSvc)).Should(BeNil())
			}

			It("Should mark the Subscription with valid sink as ready", func() {
				testCreatingSubscription(sink)
			})
			It("Should mark the Subscription with valid sink with the port suffix as ready", func() {
				testCreatingSubscription(sink + ":8080")
			})
			It("Should mark the Subscription with valid sink with the port suffix and path as ready", func() {
				testCreatingSubscription(sink + ":8080" + "/myEndpoint")
			})
		})

		When("Subscription With invalid sink", func() {
			invalidSinkMsgCheck := func(sink, subConditionMsg, k8sEventMsg string) {
				ctx := context.Background()
				cancel = startReconciler(tc.eventTypePrefix, defaultSinkValidator, natsURL)
				defer cancel()
				subscriptionName := fmt.Sprintf(subscriptionNameFormat, id)

				// Create subscription
				givenSubscription := reconcilertesting.NewSubscription(subscriptionName, namespaceName, reconcilertesting.WithSpecificEventTypeFilter(reconcilertesting.EventSource, tc.eventTypeToSubscribe), reconcilertesting.WithWebhookForNats)
				givenSubscription.Spec.Sink = sink
				byEnsuringSubscriptionCreated(ctx, givenSubscription)

				getSubscription(ctx, givenSubscription).Should(And(
					reconcilertesting.HaveSubscriptionName(subscriptionName),
					reconcilertesting.HaveCondition(eventingv1alpha1.MakeCondition(
						eventingv1alpha1.ConditionSubscriptionActive,
						eventingv1alpha1.ConditionReasonNATSSubscriptionActive,
						v1.ConditionFalse, subConditionMsg)),
				))

				var subscriptionEvents = v1.EventList{}
				subscriptionEvent := v1.Event{
					Reason:  string(events.ReasonValidationFailed),
					Message: k8sEventMsg,
					Type:    v1.EventTypeWarning,
				}
				getK8sEvents(&subscriptionEvents, givenSubscription.Namespace).Should(reconcilertesting.HaveEvent(subscriptionEvent))

				Expect(k8sClient.Delete(ctx, givenSubscription)).Should(BeNil())
				isSubscriptionDeleted(ctx, givenSubscription).Should(reconcilertesting.HaveNotFoundSubscription(true))
			}

			It("Should mark the Subscription as not ready if sink URL scheme is not 'http' or 'https'", func() {
				invalidSinkMsgCheck(
					"invalid",
					"sink URL scheme should be 'http' or 'https'",
					"Sink URL scheme should be HTTP or HTTPS: invalid",
				)
			})
			It("Should mark the Subscription as not ready if sink contains invalid characters", func() {
				invalidSinkMsgCheck(
					"http://127.0.0. 1",
					"not able to parse sink url with error: parse \"http://127.0.0. 1\": invalid character \" \" in host name",
					"Not able to parse Sink URL with error: parse \"http://127.0.0. 1\": invalid character \" \" in host name",
				)
			})
			It("Should mark the Subscription as not ready if sink does not contain suffix 'svc.cluster.local'", func() {
				invalidSinkMsgCheck(
					"http://127.0.0.1",
					"sink does not contain suffix: svc.cluster.local in the URL",
					"Sink does not contain suffix: svc.cluster.local",
				)
			})
			It("Should mark the Subscription as not ready if sink does not contain 5 sub-domains", func() {
				invalidSinkMsgCheck(
					fmt.Sprintf("https://%s.%s.%s.svc.cluster.local", "testapp", "testsub", "test"),
					"sink should contain 5 sub-domains: testapp.testsub.test.svc.cluster.local",
					"Sink should contain 5 sub-domains: testapp.testsub.test.svc.cluster.local",
				)
			})
			It("Should mark the Subscription as not ready if sink points to different namespace", func() {
				invalidSinkMsgCheck(
					fmt.Sprintf("https://%s.%s.svc.cluster.local", "testapp", "test-ns"),
					"namespace of subscription: test and the namespace of subscriber: test-ns are different",
					"Namespace of subscription: test and the subscriber: test-ns are different",
				)
			})
			It("Should mark the Subscription as not ready if sink is not a valid cluster local service", func() {
				invalidSinkMsgCheck(
					reconcilertesting.GetValidSink(namespaceName, "testapp"),
					"sink is not valid cluster local svc, failed with error: Service \"testapp\" not found",
					"Sink does not correspond to a valid cluster local svc",
				)
			})
		})

		When("Create Subscription with empty protocol, protocolsettings and dialect", func() {
			It("Should mark the Subscription as ready", func() {
				ctx := context.Background()
				cancel = startReconciler(tc.eventTypePrefix, defaultSinkValidator, natsURL)
				defer cancel()
				subscriptionName := fmt.Sprintf(subscriptionNameFormat, id)
				subscriberName := fmt.Sprintf(subscriberNameFormat, id)

				// create subscriber svc
				subscriberSvc := reconcilertesting.NewSubscriberSvc(subscriberName, namespaceName)
				byEnsuringSubscriberSvcCreated(ctx, subscriberSvc)

				// create subscription
				subscription := reconcilertesting.NewSubscription(subscriptionName, namespaceName, reconcilertesting.WithSpecificEventTypeFilter("", tc.eventTypeToSubscribe))
				reconcilertesting.SetValidSink(namespaceName, subscriberSvc.Name, subscription)
				byEnsuringSubscriptionCreated(ctx, subscription)

				getSubscription(ctx, subscription).Should(And(
					reconcilertesting.HaveSubscriptionName(subscriptionName),
					reconcilertesting.HaveCondition(eventingv1alpha1.MakeCondition(
						eventingv1alpha1.ConditionSubscriptionActive,
						eventingv1alpha1.ConditionReasonNATSSubscriptionActive,
						v1.ConditionTrue, "")),
				))

				// check for subscription at nats
				backendSubscription := getSubscriptionFromNats(natsBackend.GetAllSubscriptions(), subscriptionName)
				Expect(backendSubscription).NotTo(BeNil())
				Expect(backendSubscription.IsValid()).To(BeTrue())
				Expect(backendSubscription.Subject).Should(Equal(tc.natsSubjectToPublish))
			})
		})

		When("Change Subscription configuration", func() {
			It("Should reflect the new config in the subscription status", func() {
				By("Creating the subscription using the default config")
				ctx := context.Background()
				cancel = startReconciler(tc.eventTypePrefix, defaultSinkValidator, natsURL)
				defer cancel()
				subscriptionName := fmt.Sprintf(subscriptionNameFormat, id)
				subscriberName := fmt.Sprintf(subscriberNameFormat, id)

				// create subscriber svc
				subscriberSvc := reconcilertesting.NewSubscriberSvc(subscriberName, namespaceName)
				byEnsuringSubscriberSvcCreated(ctx, subscriberSvc)

				// create subscription
				subscription := reconcilertesting.NewSubscription(subscriptionName, namespaceName, reconcilertesting.WithSpecificEventTypeFilter(reconcilertesting.EventSource, tc.eventTypeToSubscribe), reconcilertesting.WithWebhookForNats)
				reconcilertesting.SetValidSink(namespaceName, subscriberSvc.Name, subscription)
				byEnsuringSubscriptionCreated(ctx, subscription)

				getSubscription(ctx, subscription).Should(And(
					reconcilertesting.HaveSubscriptionName(subscriptionName),
					reconcilertesting.HaveCondition(eventingv1alpha1.MakeCondition(
						eventingv1alpha1.ConditionSubscriptionActive,
						eventingv1alpha1.ConditionReasonNATSSubscriptionActive,
						v1.ConditionTrue, "")),
					reconcilertesting.HaveSubsConfiguration(&eventingv1alpha1.SubscriptionConfig{
						MaxInFlightMessages: defaultSubsConfig.MaxInFlightMessages,
					}),
				))

				By("Updating the subscription configuration in the spec")

				newMaxInFlight := defaultSubsConfig.MaxInFlightMessages + 1
				changedSub := subscription.DeepCopy()
				changedSub.Spec.Config = &eventingv1alpha1.SubscriptionConfig{
					MaxInFlightMessages: newMaxInFlight,
				}
				Expect(k8sClient.Update(ctx, changedSub)).Should(BeNil())

				Eventually(subscriptionGetter(ctx, subscription.Name, subscription.Namespace), timeout, pollingInterval).
					Should(And(
						reconcilertesting.HaveSubscriptionName(subscriptionName),
						reconcilertesting.HaveCondition(eventingv1alpha1.MakeCondition(
							eventingv1alpha1.ConditionSubscriptionActive,
							eventingv1alpha1.ConditionReasonNATSSubscriptionActive,
							v1.ConditionTrue, ""),
						),
						reconcilertesting.HaveSubsConfiguration(&eventingv1alpha1.SubscriptionConfig{
							MaxInFlightMessages: newMaxInFlight,
						}),
					))

				// check for subscription at nats
				backendSubscription := getSubscriptionFromNats(natsBackend.GetAllSubscriptions(), subscriptionName)
				Expect(backendSubscription).NotTo(BeNil())
				Expect(backendSubscription.IsValid()).To(BeTrue())
				Expect(backendSubscription.Subject).Should(Equal(tc.natsSubjectToPublish))

				Expect(k8sClient.Delete(ctx, subscription)).Should(BeNil())
				isSubscriptionDeleted(ctx, subscription).Should(reconcilertesting.HaveNotFoundSubscription(true))
			})
		})

		When("Create Subscription with empty event type", func() {
			It("Should mark the subscription as not ready", func() {
				ctx := context.Background()
				cancel = startReconciler(tc.eventTypePrefix, defaultSinkValidator, natsURL)
				defer cancel()
				subscriptionName := fmt.Sprintf(subscriptionNameFormat, id)
				subscriberName := fmt.Sprintf(subscriberNameFormat, id)

				// create subscriber svc
				subscriberSvc := reconcilertesting.NewSubscriberSvc(subscriberName, namespaceName)
				byEnsuringSubscriberSvcCreated(ctx, subscriberSvc)

				// Create subscription
				givenSubscription := reconcilertesting.NewSubscription(subscriptionName, namespaceName, reconcilertesting.WithSpecificEventTypeFilter(reconcilertesting.EventSource, ""), reconcilertesting.WithWebhookForNats)
				reconcilertesting.SetValidSink(namespaceName, subscriberName, givenSubscription)
				byEnsuringSubscriptionCreated(ctx, givenSubscription)

				getSubscription(ctx, givenSubscription).Should(And(
					reconcilertesting.HaveSubscriptionName(subscriptionName),
					reconcilertesting.HaveCondition(eventingv1alpha1.MakeCondition(
						eventingv1alpha1.ConditionSubscriptionActive,
						eventingv1alpha1.ConditionReasonNATSSubscriptionActive,
						v1.ConditionFalse, nats.ErrBadSubject.Error())),
				))
			})
		})

		When("Sending Events through Dispatcher for multiple subscribers", func() {
			It("Should receive events in subscribers", func() {
				ctx := context.Background()

				// Start reconciler with empty checkSink function
				cancel = startReconciler(tc.eventTypePrefix, func(ctx context.Context, r *Reconciler, subscription *eventingv1alpha1.Subscription) error {
					return nil
				}, natsURL)
				defer cancel()

				subName1 := fmt.Sprintf(subscriptionNameFormat, id)
				subName2 := fmt.Sprintf("subb-%d", id)

				publishToSubjects := []string{
					fmt.Sprintf("%s0", tc.natsSubjectToPublish),
					fmt.Sprintf("%s1", tc.natsSubjectToPublish),
				}

				subscribeToEventTypes := []string{
					fmt.Sprintf("%s0", tc.eventTypeToSubscribe),
					fmt.Sprintf("%s1", tc.eventTypeToSubscribe),
				}

				// create subscribers
				subChan1 := make(chan []byte)
				url1, shutdown := newSubscriber(subChan1)
				defer shutdown()

				subChan2 := make(chan []byte)
				url2, shutdown2 := newSubscriber(subChan2)
				defer shutdown2()

				// create subscription
				subscription1 := reconcilertesting.NewSubscription(subName1, namespaceName, reconcilertesting.WithSpecificEventTypeFilter(reconcilertesting.EventSource, subscribeToEventTypes[0]), reconcilertesting.WithWebhookForNats)
				subscription2 := reconcilertesting.NewSubscription(subName2, namespaceName, reconcilertesting.WithSpecificEventTypeFilter(reconcilertesting.EventSource, subscribeToEventTypes[1]), reconcilertesting.WithWebhookForNats)

				// assign sink URL
				subscription1.Spec.Sink = url1
				subscription2.Spec.Sink = url2

				// ensure subscription is created
				byEnsuringSubscriptionCreated(ctx, subscription1)
				byEnsuringSubscriptionCreated(ctx, subscription2)

				// retrieve subscription and check whether it is ready
				getSubscription(ctx, subscription1).Should(And(
					reconcilertesting.HaveSubscriptionName(subName1),
					reconcilertesting.HaveCondition(eventingv1alpha1.MakeCondition(
						eventingv1alpha1.ConditionSubscriptionActive,
						eventingv1alpha1.ConditionReasonNATSSubscriptionActive,
						v1.ConditionTrue, "")),
					reconcilertesting.HaveSubsConfiguration(&eventingv1alpha1.SubscriptionConfig{
						MaxInFlightMessages: defaultSubsConfig.MaxInFlightMessages,
					}),
				))

				getSubscription(ctx, subscription2).Should(And(
					reconcilertesting.HaveSubscriptionName(subName2),
					reconcilertesting.HaveCondition(eventingv1alpha1.MakeCondition(
						eventingv1alpha1.ConditionSubscriptionActive,
						eventingv1alpha1.ConditionReasonNATSSubscriptionActive,
						v1.ConditionTrue, "")),
					reconcilertesting.HaveSubsConfiguration(&eventingv1alpha1.SubscriptionConfig{
						MaxInFlightMessages: defaultSubsConfig.MaxInFlightMessages,
					}),
				))

				// establish connection with NATS
				connection, err := connectToNats(natsURL)
				Expect(err).ShouldNot(HaveOccurred())

				// publish events to nats
				err = connection.Publish(publishToSubjects[0], []byte(reconcilertesting.StructuredCloudEvent))
				Expect(err).ShouldNot(HaveOccurred())

				err = connection.Publish(publishToSubjects[1], []byte(reconcilertesting.StructuredCloudEventUpdated))
				Expect(err).ShouldNot(HaveOccurred())

				// make sure that the subscriber received the message
				sent := fmt.Sprintf(`"%s"`, reconcilertesting.EventData)
				Eventually(func() ([]byte, error) {
					return getFromChanOrTimeout(subChan1, smallPollingInterval)
				}, timeout, pollingInterval).Should(WithTransform(bytesStringer, Equal(sent)))

				Eventually(func() ([]byte, error) {
					return getFromChanOrTimeout(subChan2, smallPollingInterval)
				}, timeout, pollingInterval).Should(WithTransform(bytesStringer, Equal(sent)))
			})
		})
	}
})

// getK8sEvents returns all kubernetes events for the given namespace.
// The result can be used in a gomega assertion.
func getK8sEvents(eventList *v1.EventList, namespace string) AsyncAssertion {
	ctx := context.TODO()
	return Eventually(func() v1.EventList {
		err := k8sClient.List(ctx, eventList, client.InNamespace(namespace))
		if err != nil {
			return v1.EventList{}
		}
		return *eventList
	}, smallTimeout, smallPollingInterval)
}

func newSubscriber(result chan []byte) (string, func()) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		result <- body
	}))
	return server.URL, server.Close
}

func connectToNats(natsURL string) (*nats.Conn, error) {
	connection, err := nats.Connect(natsURL, nats.RetryOnFailedConnect(true), nats.MaxReconnects(3), nats.ReconnectWait(time.Second))
	if err != nil {
		return nil, err
	}
	if connection.Status() != nats.CONNECTED {
		return nil, err
	}
	return connection, nil
}

func getFromChanOrTimeout(ch <-chan []byte, t time.Duration) ([]byte, error) {
	select {
	case received := <-ch:
		return received, nil
	case <-time.After(t):
		return nil, fmt.Errorf("timed out waiting for a message")
	}
}

func bytesStringer(bs []byte) string {
	return string(bs)
}

func byEnsuringSubscriptionCreated(ctx context.Context, subscription *eventingv1alpha1.Subscription) {
	By(fmt.Sprintf("Ensuring the test namespace %q is created", subscription.Namespace))
	if subscription.Namespace != "default " {
		// create testing namespace
		namespace := fixtureNamespace(subscription.Namespace)
		if namespace.Name != "default" {
			err := k8sClient.Create(ctx, namespace)
			if !k8serrors.IsAlreadyExists(err) {
				fmt.Println(err)
				Expect(err).ShouldNot(HaveOccurred())
			}
		}
	}

	By(fmt.Sprintf("Ensuring the subscription %q is created", subscription.Name))
	// create subscription
	err := k8sClient.Create(ctx, subscription)
	Expect(err).Should(BeNil())
}

func byEnsuringSubscriptionUpdated(ctx context.Context, subscription *eventingv1alpha1.Subscription) {
	By(fmt.Sprintf("Ensuring the subscription %q is updated", subscription.Name))
	// create subscription
	err := k8sClient.Update(ctx, subscription)
	Expect(err).Should(BeNil())
}

func fixtureNamespace(name string) *v1.Namespace {
	namespace := v1.Namespace{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Namespace",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	return &namespace
}

func subscriptionGetter(ctx context.Context, name, namespace string) func() (*eventingv1alpha1.Subscription, error) {
	return func() (*eventingv1alpha1.Subscription, error) {
		lookupKey := types.NamespacedName{
			Namespace: namespace,
			Name:      name,
		}
		subscription := &eventingv1alpha1.Subscription{}
		if err := k8sClient.Get(ctx, lookupKey, subscription); err != nil {
			return &eventingv1alpha1.Subscription{}, err
		}
		return subscription, nil
	}
}

// getSubscription fetches a subscription using the lookupKey and allows making assertions on it
func getSubscription(ctx context.Context, subscription *eventingv1alpha1.Subscription, intervals ...interface{}) AsyncAssertion {
	if len(intervals) == 0 {
		intervals = []interface{}{smallTimeout, smallPollingInterval}
	}
	return Eventually(func() *eventingv1alpha1.Subscription {
		lookupKey := types.NamespacedName{
			Namespace: subscription.Namespace,
			Name:      subscription.Name,
		}
		if err := k8sClient.Get(ctx, lookupKey, subscription); err != nil {
			return &eventingv1alpha1.Subscription{}
		}
		return subscription
	}, intervals...)
}

// isSubscriptionDeleted checks a subscription is deleted and allows making assertions on it
func isSubscriptionDeleted(ctx context.Context, subscription *eventingv1alpha1.Subscription) AsyncAssertion {
	return Eventually(func() bool {
		lookupKey := types.NamespacedName{
			Namespace: subscription.Namespace,
			Name:      subscription.Name,
		}
		if err := k8sClient.Get(ctx, lookupKey, subscription); err != nil {
			return k8serrors.IsNotFound(err)
		}
		return false
	}, smallTimeout, smallPollingInterval)
}

////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
// Test Suite setup ////////////////////////////////////////////////////////////////////////////////////////////////////
////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

// These tests use Ginkgo (BDD-style Go controllertesting framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

// TODO: make configurable
const (
	useExistingCluster       = false
	attachControlPlaneOutput = false
)

var testID int
var natsURL string
var cfg *rest.Config
var k8sClient client.Client
var testEnv *envtest.Environment
var natsServer *natsserver.Server
var defaultSubsConfig = env.DefaultSubscriptionConfig{MaxInFlightMessages: 1, DispatcherRetryPeriod: time.Second, DispatcherMaxRetries: 1}
var reconciler *Reconciler
var natsBackend *handlers.Nats
var cancel context.CancelFunc

func TestAPIs(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecsWithDefaultAndCustomReporters(t, "NATS Controller Suite", []Reporter{printer.NewlineReporter{}})
}

var _ = BeforeSuite(func(done Done) {
	By("bootstrapping test environment")
	natsServer, natsURL = startNATS(natsPort)
	useExistingCluster := useExistingCluster
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("../../../", "config", "crd", "bases"),
			filepath.Join("../../../", "config", "crd", "external"),
		},
		AttachControlPlaneOutput: attachControlPlaneOutput,
		UseExistingCluster:       &useExistingCluster,
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).ToNot(HaveOccurred())
	Expect(cfg).ToNot(BeNil())

	close(done)
}, 60)

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	reconcilertesting.ShutDownNATSServer(natsServer)
	err := testEnv.Stop()
	Expect(err).ToNot(HaveOccurred())
}, 60)

func startNATS(port int) (*natsserver.Server, string) {
	natsServer := reconcilertesting.RunNatsServerOnPort(port)
	clientURL := natsServer.ClientURL()
	log.Printf("NATS server started %v", clientURL)
	return natsServer, clientURL
}

func startReconciler(eventTypePrefix string, sinkValidator sinkValidator, natsURL string) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	logf.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(GinkgoWriter)))

	err := eventingv1alpha1.AddToScheme(scheme.Scheme)
	It("should add to scheme without error", func() {
		Expect(err).NotTo(HaveOccurred())
	})

	syncPeriod := time.Second * 2
	k8sManager, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:             scheme.Scheme,
		SyncPeriod:         &syncPeriod,
		MetricsBindAddress: ":7070",
	})
	It("should create new manager without error", func() {
		Expect(err).ToNot(HaveOccurred())
	})

	envConf := env.NatsConfig{
		URL:             natsURL,
		MaxReconnects:   10,
		ReconnectWait:   time.Second,
		EventTypePrefix: eventTypePrefix,
	}

	// prepare application-lister
	app := applicationtest.NewApplication(reconcilertesting.ApplicationNameNotClean, nil)
	applicationLister := fake.NewApplicationListerOrDie(context.Background(), app)

	defaultLogger, err := logger.New(string(kymalogger.JSON), string(kymalogger.INFO))
	It("should create logger without error", func() {
		Expect(err).To(BeNil())
	})

	reconciler = NewReconciler(
		ctx,
		k8sManager.GetClient(),
		applicationLister,
		k8sManager.GetCache(),
		defaultLogger,
		k8sManager.GetEventRecorderFor("eventing-controller-nats"),
		envConf,
		defaultSubsConfig,
	)
	reconciler.sinkValidator = sinkValidator

	err = reconciler.SetupUnmanaged(k8sManager)
	It("should create controller without error", func() {
		Expect(err).ToNot(HaveOccurred())
	})

	natsBackend = reconciler.Backend.(*handlers.Nats)

	go func() {
		defer GinkgoRecover()
		err = k8sManager.Start(ctx)
		It("should start the manager without error", func() {
			Expect(err).ToNot(HaveOccurred())
		})
	}()

	k8sClient = k8sManager.GetClient()
	It("should get K8s client without error", func() {
		Expect(k8sClient).ToNot(BeNil())
	})

	return cancel
}

// itShouldHaveCreatedSubscriberService creates a Service in the k8s cluster. If a custom namespace is used, it will be created as well.
func itShouldHaveCreatedSubscriberService(ctx context.Context, svc *v1.Service) {
	It("should have created the subscriber service", func() {
		By(fmt.Sprintf("Ensuring the test namespace %q is created", svc.Namespace))
		if svc.Namespace != "default " {
			// create testing namespace
			namespace := fixtureNamespace(svc.Namespace)
			if namespace.Name != "default" {
				err := k8sClient.Create(ctx, namespace)
				if !k8serrors.IsAlreadyExists(err) {
					fmt.Println(err)
					Expect(err).ShouldNot(HaveOccurred())
				}
			}
		}

		By(fmt.Sprintf("Ensuring the subscriber service %q is created", svc.Name))
		// create subscription
		err := k8sClient.Create(ctx, svc)
		Expect(err).Should(BeNil())
	})
}

// byEnsuringSubscriberSvcCreated creates a Service in the k8s cluster. If a custom namespace is used, it will be created as well.
func byEnsuringSubscriberSvcCreated(ctx context.Context, svc *v1.Service) { //todo remove this when all useses replaced
	By(fmt.Sprintf("Ensuring the test namespace %q is created", svc.Namespace))
	if svc.Namespace != "default " {
		// create testing namespace
		namespace := fixtureNamespace(svc.Namespace)
		if namespace.Name != "default" {
			err := k8sClient.Create(ctx, namespace)
			if !k8serrors.IsAlreadyExists(err) {
				fmt.Println(err)
				Expect(err).ShouldNot(HaveOccurred())
			}
		}
	}

	By(fmt.Sprintf("Ensuring the subscriber service %q is created", svc.Name))
	// create subscription
	err := k8sClient.Create(ctx, svc)
	Expect(err).Should(BeNil())
}

func getSubscriptionFromNats(subscriptionMap map[string]*nats.Subscription, subscriptionName string) *nats.Subscription {
	i := 0
	for key, subscription := range subscriptionMap {
		if strings.Contains(key, subscriptionName) {
			return subscription
		}
		i++
	}
	return nil
}
