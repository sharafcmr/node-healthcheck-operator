package utils

import (
	"context"

	"github.com/go-logr/logr"
	remediationv1alpha1 "github.com/medik8s/node-healthcheck-operator/api/v1alpha1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// NHCByNodeMapperFunc return the Node-to-NHC mapper function
func NHCByNodeMapperFunc(c client.Client, logger logr.Logger) handler.MapFunc {
	// This closure is meant to fetch all NHC to fill the reconcile queue.
	// If we have multiple nhc then it is possible that we fetch nhc objects that
	// are unrelated to this node. Its even possible that the node still doesn't
	// have the right labels set to be picked up by the nhc selector.
	delegate := func(o client.Object) []reconcile.Request {
		var nhcList remediationv1alpha1.NodeHealthCheckList
		err := c.List(context.Background(), &nhcList, &client.ListOptions{})
		if err != nil {
			return nil
		}
		var r []reconcile.Request
		for _, nhc := range nhcList.Items {
			selector, err := metav1.LabelSelectorAsSelector(&nhc.Spec.Selector)
			if err != nil {
				logger.Error(err, "failed to use the NHC selector.")
			}

			node := &v1.Node{}
			err = c.Get(context.Background(), client.ObjectKey{Name: o.GetName()}, node)
			if selector.Matches(labels.Set(node.GetLabels())) {
				r = append(r, reconcile.Request{NamespacedName: types.NamespacedName{Name: nhc.GetName()}})
			}
		}
		return r
	}
	return delegate
}

// MHCByNodeMapperFunc return the Node-to-MHC mapper function
func MHCByNodeMapperFunc(c client.Client, logger logr.Logger) handler.MapFunc {
	// Find all MHC which target the given Node
	delegate := func(o client.Object) []reconcile.Request {
		var mhcList machinev1beta1.MachineHealthCheckList
		err := c.List(context.Background(), &mhcList, &client.ListOptions{})
		if err != nil {
			return nil
		}
		var r []reconcile.Request
		for _, mhc := range mhcList.Items {
			selector, err := metav1.LabelSelectorAsSelector(&mhc.Spec.Selector)
			if err != nil {
				logger.Error(err, "failed to use the MHC selector.")
				continue
			}

			node := &v1.Node{}
			err = c.Get(context.Background(), client.ObjectKey{Name: o.GetName()}, node)
			if selector.Matches(labels.Set(node.GetLabels())) {
				r = append(r, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: mhc.GetNamespace(), Name: mhc.GetName()}})
			}
		}
		return r
	}
	return delegate
}
