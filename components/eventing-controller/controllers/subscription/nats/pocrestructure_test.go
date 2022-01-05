package nats

import (
	"context"
	"fmt"
	eventingv1alpha1 "github.com/kyma-project/kyma/components/eventing-controller/api/v1alpha1"
	reconcilertesting "github.com/kyma-project/kyma/components/eventing-controller/testing"
	. "github.com/onsi/ginkgo"
	v1 "k8s.io/api/core/v1"

	. "github.com/onsi/gomega"
)

var _ = Describe("Subscription Reconciliation Tests", func() {
	var id int

	BeforeEach(func() {
		id++
	})

	testCases := []struct {
		context, eventTypePrefix, natsSubjectToPublish, eventTypeToSubscribe string
	}{
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
	for _, testCase := range testCases {
		Context(testCase.context, func() {
			When("Create/Delete Subscription", func() {
				It("Should create/delete NATS Subscription", func() {
					ctx := context.Background()
					cancel = startReconciler(testCase.eventTypePrefix, defaultSinkValidator)
					defer cancel()
					subscriptionName := fmt.Sprintf(subscriptionNameFormat, id)
					subscriberName := fmt.Sprintf(subscriberNameFormat, id)

					// create subscriber svc
					subscriberSvc := reconcilertesting.NewSubscriberSvc(subscriberName, namespaceName)
					ensureSubscriberSvcCreated(ctx, subscriberSvc)

					// create subscription
					optFilter := reconcilertesting.WithFilter(reconcilertesting.EventSource, testCase.eventTypeToSubscribe)
					optWebhook := reconcilertesting.WithWebhookForNats
					subscription := reconcilertesting.NewSubscription(subscriptionName, namespaceName, optFilter, optWebhook)
					reconcilertesting.WithValidSink(namespaceName, subscriberSvc.Name, subscription)
					ensureSubscriptionCreated(ctx, subscription)

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
					Expect(backendSubscription.Subject).Should(Equal(testCase.natsSubjectToPublish))

					Expect(k8sClient.Delete(ctx, subscription)).Should(BeNil())
					isSubscriptionDeleted(ctx, subscription).Should(reconcilertesting.HaveNotFoundSubscription(true))
				})
			})
		})
	}
})
