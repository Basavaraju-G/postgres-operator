/*
 Copyright 2021 Crunchy Data Solutions, Inc.
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

package postgrescluster

import (
	"context"
	"time"

	"github.com/pkg/errors"
	"go.opentelemetry.io/otel/trace"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/crunchydata/postgres-operator/internal/initialize"
	"github.com/crunchydata/postgres-operator/internal/logging"
	"github.com/crunchydata/postgres-operator/internal/naming"
	"github.com/crunchydata/postgres-operator/internal/patroni"
	"github.com/crunchydata/postgres-operator/internal/pgbackrest"
	"github.com/crunchydata/postgres-operator/internal/pki"
	"github.com/crunchydata/postgres-operator/internal/postgres"
	"github.com/crunchydata/postgres-operator/pkg/apis/postgres-operator.crunchydata.com/v1beta1"
)

// +kubebuilder:rbac:groups="",resources=pods,verbs=list
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=patch

// deleteInstances gracefully stops instances of cluster to avoid failovers and
// unclean shutdowns of PostgreSQL. It returns (nil, nil) when finished.
func (r *Reconciler) deleteInstances(
	ctx context.Context, cluster *v1beta1.PostgresCluster,
) (*reconcile.Result, error) {
	// Find all instance pods to determine which to shutdown and in what order.
	pods := &v1.PodList{}
	instances, err := naming.AsSelector(naming.ClusterInstances(cluster.Name))
	if err == nil {
		err = errors.WithStack(
			r.Client.List(ctx, pods,
				client.InNamespace(cluster.Namespace),
				client.MatchingLabelsSelector{Selector: instances},
			))
	}
	if err != nil {
		return nil, err
	}

	if len(pods.Items) == 0 {
		// There are no instances, so there's nothing to do.
		// The caller can do what they like.
		return nil, nil
	}

	// There are some instances, so the caller should at least wait for further
	// events.
	result := reconcile.Result{}

	// stop schedules pod for deletion by scaling its controller to zero.
	stop := func(pod *v1.Pod) error {
		instance := &unstructured.Unstructured{}
		instance.SetNamespace(cluster.Namespace)

		switch owner := metav1.GetControllerOfNoCopy(pod); {
		case owner == nil:
			return errors.Errorf("pod %q has no owner", client.ObjectKeyFromObject(pod))

		case owner.Kind == "StatefulSet":
			instance.SetAPIVersion(owner.APIVersion)
			instance.SetKind(owner.Kind)
			instance.SetName(owner.Name)

		default:
			return errors.Errorf("unexpected kind %q", owner.Kind)
		}

		// apps/v1.Deployment, apps/v1.ReplicaSet, and apps/v1.StatefulSet all
		// have a "spec.replicas" field with the same meaning.
		patch := client.RawPatch(client.Merge.Type(), []byte(`{"spec":{"replicas":0}}`))
		err := errors.WithStack(r.patch(ctx, instance, patch))

		// When the pod controller is missing, requeue rather than return an
		// error. The garbage collector will stop the pod, and it is not our
		// mistake that something else is deleting objects. Use RequeueAfter to
		// avoid being rate-limited due to a deluge of delete events.
		if err != nil {
			result.RequeueAfter = 10 * time.Second
		}
		return client.IgnoreNotFound(err)
	}

	if len(pods.Items) == 1 {
		// There's one instance; stop it.
		return &result, stop(&pods.Items[0])
	}

	// There are multiple instances; stop the replicas. When none are found,
	// requeue to try again.

	result.Requeue = true
	for i := range pods.Items {
		role := pods.Items[i].Labels[naming.LabelRole]
		if err == nil && role == naming.RolePatroniReplica {
			err = stop(&pods.Items[i])
			result.Requeue = false
		}

		// An instance without a role label is not participating in the Patroni
		// cluster. It may be unhealthy or has not yet (re-)joined. Go ahead and
		// stop these as well.
		if err == nil && len(role) == 0 {
			err = stop(&pods.Items[i])
			result.Requeue = false
		}
	}

	return &result, err
}

// deleteInstance will delete all resources related to a single instance
func (r *Reconciler) deleteInstance(
	ctx context.Context,
	cluster *v1beta1.PostgresCluster,
	instanceName string,
) error {
	log := logging.FromContext(ctx)
	instanceResources, err := r.getInstanceResources(cluster, instanceName)
	if err != nil {
		log.Error(err, "unable to get instance resources for PostgresCluster")
		return err
	}

	for i := range instanceResources {
		err = errors.WithStack(r.deleteControlled(ctx, cluster, &instanceResources[i]))
		if err != nil {
			return err
		}
	}

	return nil
}

// getInstanceResources returns list of unstructured resources associated with
// an instance
func (r *Reconciler) getInstanceResources(
	cluster *v1beta1.PostgresCluster,
	instanceName string,
) ([]unstructured.Unstructured, error) {
	owned := []unstructured.Unstructured{}

	gvks := []schema.GroupVersionKind{{
		Group:   v1.SchemeGroupVersion.Group,
		Version: v1.SchemeGroupVersion.Version,
		Kind:    "ConfigMapList",
	}, {
		Group:   v1.SchemeGroupVersion.Group,
		Version: v1.SchemeGroupVersion.Version,
		Kind:    "SecretList",
	}, {
		Group:   appsv1.SchemeGroupVersion.Group,
		Version: appsv1.SchemeGroupVersion.Version,
		Kind:    "StatefulSetList",
	}, {
		Group:   v1.SchemeGroupVersion.Group,
		Version: v1.SchemeGroupVersion.Version,
		Kind:    "PersistentVolumeClaimList",
	}}

	selector, err := naming.AsSelector(naming.ClusterInstance(cluster.Name, instanceName))
	if err == nil {
		for _, gvk := range gvks {
			uList := &unstructured.UnstructuredList{}
			uList.SetGroupVersionKind(gvk)
			if err := r.Client.List(context.Background(), uList,
				client.InNamespace(cluster.GetNamespace()),
				client.MatchingLabelsSelector{Selector: selector}); err != nil {
				return nil, errors.WithStack(err)
			}
			if len(uList.Items) == 0 {
				continue
			}

			for i, u := range uList.Items {
				if metav1.IsControlledBy(&uList.Items[i], cluster) {
					owned = append(owned, u)
				}
			}
		}
	}

	return owned, nil
}

// reconcileInstanceSets reconciles instance sets in the environment to match
// the current spec. This is done by scaling up or down instances where necessary
func (r *Reconciler) reconcileInstanceSets(
	ctx context.Context,
	cluster *v1beta1.PostgresCluster,
	clusterConfigMap *v1.ConfigMap,
	rootCA *pki.RootCertificateAuthority,
	clusterPodService *v1.Service,
	instanceServiceAccount *v1.ServiceAccount,
	patroniLeaderService *v1.Service,
	primaryCertificate *v1.SecretProjection,
) ([]string, error) {
	instanceNames := []string{}

	// Range over instance sets to scale up and ensure that each set has
	// at least the number of replicas defined in the spec. The set can
	// have more replicas than defined
	for i := range cluster.Spec.InstanceSets {
		instanceSet, err := r.scaleUpInstances(
			ctx, cluster, &cluster.Spec.InstanceSets[i],
			clusterConfigMap, rootCA, clusterPodService,
			instanceServiceAccount, patroniLeaderService,
			primaryCertificate)
		if err != nil {
			return nil, err
		}

		for _, instance := range instanceSet.Items {
			instanceNames = append(instanceNames, instance.GetName())
		}
	}

	// Scaledown is called on the whole cluster in order to consider all
	// instances. This is necessary because we have no way to determine
	// which instance or instance set contains the primary pod.
	err := r.scaleDownInstances(ctx, cluster)
	if err != nil {
		return nil, err
	}

	return instanceNames, nil
}

// scaleDownInstances removes extra instances from a cluster until it matches
// the spec. This function can delete the primary instance and force the
// cluster to failover under two conditions:
// - If the instance set that contains the primary instance is removed from
//   the spec
// - If the instance set that contains the primary instance is updated to
//   have 0 replicas
// If either of these conditions are met then the primary instance will be
// marked for deletion and deleted after all other instances
func (r *Reconciler) scaleDownInstances(
	ctx context.Context,
	cluster *v1beta1.PostgresCluster,
) error {
	pods := &v1.PodList{}
	selector, err := naming.AsSelector(naming.ClusterInstances(cluster.Name))
	if err == nil {
		err = errors.WithStack(
			r.Client.List(ctx, pods,
				client.InNamespace(cluster.Namespace),
				client.MatchingLabelsSelector{Selector: selector},
			))
	}
	if err != nil {
		return err
	}

	want := map[string]int{}
	for _, set := range cluster.Spec.InstanceSets {
		want[set.Name] = int(*set.Replicas)
	}

	namesToKeep := sets.NewString()
	for _, pod := range podsToKeep(pods.Items, want) {
		namesToKeep.Insert(pod.Labels[naming.LabelInstance])
	}

	for _, pod := range pods.Items {
		if !namesToKeep.Has(pod.Labels[naming.LabelInstance]) {
			err := r.deleteInstance(ctx, cluster, pod.Labels[naming.LabelInstance])
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// podsToKeep takes a list of pods and a map containing
// the number of replicas we want for each instance set
// then returns a list of the pods that we want to keep
func podsToKeep(instances []v1.Pod, want map[string]int) []v1.Pod {

	f := func(instances []v1.Pod, want int) []v1.Pod {
		keep := []v1.Pod{}

		if want > 0 {
			for _, instance := range instances {
				if instance.Labels[naming.LabelRole] == "master" {
					keep = append(keep, instance)
				}
			}
		}

		for _, instance := range instances {
			if instance.Labels[naming.LabelRole] != "master" && len(keep) < want {
				keep = append(keep, instance)
			}
		}

		return keep
	}

	keepPodList := []v1.Pod{}
	for name, num := range want {
		list := []v1.Pod{}
		for _, instance := range instances {
			if instance.Labels[naming.LabelInstanceSet] == name {
				list = append(list, instance)
			}
		}
		keepPodList = append(keepPodList, f(list, num)...)
	}

	return keepPodList

}

// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=list

// scaleUpInstances updates the cluster until the number of instances matches
// the cluster spec
func (r *Reconciler) scaleUpInstances(
	ctx context.Context,
	cluster *v1beta1.PostgresCluster,
	set *v1beta1.PostgresInstanceSetSpec,
	clusterConfigMap *v1.ConfigMap,
	rootCA *pki.RootCertificateAuthority,
	clusterPodService *v1.Service,
	instanceServiceAccount *v1.ServiceAccount,
	patroniLeaderService *v1.Service,
	primaryCertificate *v1.SecretProjection,
) (*appsv1.StatefulSetList, error) {
	log := logging.FromContext(ctx)

	instances := &appsv1.StatefulSetList{}
	selector, err := naming.AsSelector(naming.ClusterInstanceSet(cluster.Name, set.Name))
	if err == nil {
		err = errors.WithStack(
			r.Client.List(ctx, instances,
				client.InNamespace(cluster.Namespace),
				client.MatchingLabelsSelector{Selector: selector},
			))
	}

	instanceNames := sets.NewString()
	for i := range instances.Items {
		instanceNames.Insert(instances.Items[i].Name)
	}

	// While there are fewer instances than specified, generate another empty one
	// and append it.
	for err == nil && len(instances.Items) < int(*set.Replicas) {
		var span trace.Span
		ctx, span = r.Tracer.Start(ctx, "generateInstanceName")
		next := naming.GenerateInstance(cluster, set)
		for instanceNames.Has(next.Name) {
			next = naming.GenerateInstance(cluster, set)
		}
		span.End()

		instanceNames.Insert(next.Name)
		instances.Items = append(instances.Items, appsv1.StatefulSet{ObjectMeta: next})
	}
	for i := range instances.Items {
		if err == nil {
			err = r.reconcileInstance(
				ctx, cluster, set, clusterConfigMap, rootCA, clusterPodService,
				instanceServiceAccount, patroniLeaderService, primaryCertificate,
				&instances.Items[i])
		}
	}
	if err == nil {
		log.V(1).Info("reconciled instance set", "instance-set", set.Name)
	}

	return instances, err
}

// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=create;patch

// reconcileInstance writes instance according to spec of cluster.
// See Reconciler.reconcileInstanceSet.
func (r *Reconciler) reconcileInstance(
	ctx context.Context,
	cluster *v1beta1.PostgresCluster,
	spec *v1beta1.PostgresInstanceSetSpec,
	clusterConfigMap *v1.ConfigMap,
	rootCA *pki.RootCertificateAuthority,
	clusterPodService *v1.Service,
	instanceServiceAccount *v1.ServiceAccount,
	patroniLeaderService *v1.Service,
	primaryCertificate *v1.SecretProjection,
	instance *appsv1.StatefulSet,
) error {
	log := logging.FromContext(ctx).WithValues("instance", instance.Name)
	ctx = logging.NewContext(ctx, log)

	existing := instance.DeepCopy()
	*instance = appsv1.StatefulSet{}
	instance.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("StatefulSet"))
	instance.Namespace, instance.Name = existing.Namespace, existing.Name

	err := errors.WithStack(r.setControllerReference(cluster, instance))

	if err == nil {
		instance.Labels = map[string]string{
			naming.LabelCluster:     cluster.Name,
			naming.LabelInstanceSet: spec.Name,
			naming.LabelInstance:    instance.Name,
		}
		instance.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{
				naming.LabelCluster:     cluster.Name,
				naming.LabelInstanceSet: spec.Name,
				naming.LabelInstance:    instance.Name,
			},
		}
		instance.Spec.Template.Labels = map[string]string{
			naming.LabelCluster:     cluster.Name,
			naming.LabelInstanceSet: spec.Name,
			naming.LabelInstance:    instance.Name,
		}

		// Don't clutter the namespace with extra ControllerRevisions.
		// The "controller-revision-hash" label still exists on the Pod.
		instance.Spec.RevisionHistoryLimit = initialize.Int32(0)

		// Give the Pod a stable DNS record based on its name.
		// - https://docs.k8s.io/concepts/workloads/controllers/statefulset/#stable-network-id
		// - https://docs.k8s.io/concepts/services-networking/dns-pod-service/#pods
		instance.Spec.ServiceName = clusterPodService.Name

		// TODO(cbandy): let our controller recreate the pod.
		// - https://docs.k8s.io/concepts/workloads/controllers/statefulset/#on-delete
		//instance.Spec.UpdateStrategy.Type = appsv1.OnDeleteStatefulSetStrategyType

		// Match the existing replica count, if any.
		if existing.Spec.Replicas != nil {
			instance.Spec.Replicas = initialize.Int32(*existing.Spec.Replicas)
		} else {
			instance.Spec.Replicas = initialize.Int32(1) // TODO(cbandy): start at zero, maybe
		}

		// Though we use a StatefulSet to keep an instance running, we only ever
		// want one Pod from it.
		if *instance.Spec.Replicas > 1 {
			*instance.Spec.Replicas = 1
		}

		// Restart containers any time they stop, die, are killed, etc.
		// - https://docs.k8s.io/concepts/workloads/pods/pod-lifecycle/#restart-policy
		instance.Spec.Template.Spec.RestartPolicy = v1.RestartPolicyAlways

		// ShareProcessNamespace makes Kubernetes' pause process PID 1 and lets
		// containers see each other's processes.
		// - https://docs.k8s.io/tasks/configure-pod-container/share-process-namespace/
		instance.Spec.Template.Spec.ShareProcessNamespace = initialize.Bool(true)

		instance.Spec.Template.Spec.ServiceAccountName = instanceServiceAccount.Name
		instance.Spec.Template.Spec.Containers = []v1.Container{
			{
				Name:      naming.ContainerDatabase,
				Image:     cluster.Spec.Image,
				Resources: spec.Resources,
				Ports: []v1.ContainerPort{
					{
						Name:          naming.PortPostgreSQL,
						ContainerPort: *cluster.Spec.Port,
						Protocol:      v1.ProtocolTCP,
					},
				},
			},
		}

		podSecurityContext := &v1.PodSecurityContext{SupplementalGroups: []int64{65534}}
		// set fsGroups if not OpenShift
		if cluster.Spec.OpenShift == nil || !*cluster.Spec.OpenShift {
			podSecurityContext.FSGroup = initialize.Int64(26)
		}
		instance.Spec.Template.Spec.SecurityContext = podSecurityContext
	}

	var (
		instanceConfigMap        *v1.ConfigMap
		instanceCertificates     *v1.Secret
		clusterReplicationSecret *v1.Secret
	)

	if err == nil {
		instanceConfigMap, err = r.reconcileInstanceConfigMap(ctx, cluster, instance)
	}
	if err == nil {
		instanceCertificates, err = r.reconcileInstanceCertificates(
			ctx, cluster, instance, rootCA)
	}
	if err == nil {
		clusterReplicationSecret, err = r.reconcileReplicationSecret(ctx, cluster, rootCA)
	}
	if err == nil {
		err = r.reconcilePGDATAVolume(ctx, cluster, spec, instance)
	}
	if err == nil {
		err = patroni.InstancePod(
			ctx, cluster, clusterConfigMap, clusterPodService, patroniLeaderService,
			instanceCertificates, instanceConfigMap, &instance.Spec.Template)
	}

	// Add pgBackRest containers, volumes, etc. to the instance Pod spec
	if err == nil {
		err = addPGBackRestToInstancePodSpec(cluster, &instance.Spec.Template, instance)
	}

	postgres.AddPGDATAInitToPod(cluster, &instance.Spec.Template)

	// copy the mounted replication client certificate files to the /tmp directory
	// and set the proper file permissions
	postgres.CopyReplicationTLS(cluster, &instance.Spec.Template)

	// Add PGDATA volume to the Pod template and then add PGDATA volume mounts for the
	// database container, and, if a repo host is enabled, the pgBackRest container
	PGDATAContainers := []string{naming.ContainerDatabase}
	PGDATAInitContainers := []string{naming.ContainerDatabasePGDATAInit, naming.ContainerClientCertInit}
	if pgbackrest.RepoHostEnabled(cluster) {
		PGDATAContainers = append(PGDATAContainers, naming.PGBackRestRepoContainerName)
	}
	if err == nil {
		err = postgres.AddPGDATAVolumeToPod(cluster, &instance.Spec.Template,
			naming.InstancePGDataVolume(instance).Name, PGDATAContainers,
			PGDATAInitContainers)
	}
	// add the cluster certificate secret volume to the pod to enable Postgres TLS connections
	if err == nil {
		err = errors.WithStack(postgres.AddCertVolumeToPod(cluster, &instance.Spec.Template,
			naming.ContainerClientCertInit, naming.ContainerDatabase, primaryCertificate,
			replicationCertSecretProjection(clusterReplicationSecret)))
	}
	// add nss_wrapper init container and add nss_wrapper env vars to the database and pgbackrest
	// containers
	if err == nil {
		addNSSWrapper(cluster, &instance.Spec.Template)
	}
	// add an emptyDir volume to the PodTemplateSpec and an associated '/tmp' volume mount to
	// all containers included within that spec
	if err == nil {
		addTMPEmptyDir(&instance.Spec.Template)
	}

	if err == nil {
		err = errors.WithStack(r.apply(ctx, instance))
	}
	if err == nil {
		log.V(1).Info("reconciled instance", "instance", instance.Name)
	}

	return err
}

// addPGBackRestToInstancePodSpec adds pgBackRest configuration to the PodTemplateSpec.  This
// includes adding an SSH sidecar if a pgBackRest repoHost is enabled per the current
// PostgresCluster spec, mounting pgBackRest repo volumes if a dedicated repository is not
// configured, and then mounting the proper pgBackRest configuration resources (ConfigMaps
// and Secrets)
func addPGBackRestToInstancePodSpec(cluster *v1beta1.PostgresCluster,
	template *v1.PodTemplateSpec, instance *appsv1.StatefulSet) error {

	addSSH := pgbackrest.RepoHostEnabled(cluster)
	dedicatedRepoEnabled := pgbackrest.DedicatedRepoHostEnabled(cluster)
	pgBackRestConfigContainers := []string{naming.ContainerDatabase}
	if addSSH {
		pgBackRestConfigContainers = append(pgBackRestConfigContainers,
			naming.PGBackRestRepoContainerName)
		if err := pgbackrest.AddSSHToPod(cluster, template, naming.ContainerDatabase); err != nil {
			return err
		}
	}
	if !dedicatedRepoEnabled {
		if err := pgbackrest.AddRepoVolumesToPod(cluster, template,
			pgBackRestConfigContainers...); err != nil {
			return err
		}
	}
	if err := pgbackrest.AddConfigsToPod(cluster, template, instance.Name+".conf",
		pgBackRestConfigContainers...); err != nil {
		return err
	}

	return nil
}

// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=create;patch

// reconcilePGDATAVolume writes instance according to spec of cluster.
// See Reconciler.reconcileInstanceSet.
func (r *Reconciler) reconcilePGDATAVolume(ctx context.Context, cluster *v1beta1.PostgresCluster,
	spec *v1beta1.PostgresInstanceSetSpec, instance *appsv1.StatefulSet) error {

	// generate metadata
	meta := naming.InstancePGDataVolume(instance)
	meta.Labels = map[string]string{
		naming.LabelCluster:     cluster.GetName(),
		naming.LabelInstanceSet: spec.Name,
		naming.LabelInstance:    instance.GetName(),
	}

	pgdataVolume := &v1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1.SchemeGroupVersion.String(),
			Kind:       "PersistentVolumeClaim",
		},
		ObjectMeta: meta,
		Spec:       spec.VolumeClaimSpec,
	}

	// set ownership references
	err := errors.WithStack(r.setControllerReference(cluster, pgdataVolume))

	if err == nil {
		err = errors.WithStack(r.apply(ctx, pgdataVolume))
	}

	return err
}

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=create;patch

// reconcileInstanceConfigMap writes the ConfigMap that contains generated
// files (etc) that apply to instance of cluster.
func (r *Reconciler) reconcileInstanceConfigMap(
	ctx context.Context, cluster *v1beta1.PostgresCluster, instance *appsv1.StatefulSet,
) (*v1.ConfigMap, error) {
	instanceConfigMap := &v1.ConfigMap{ObjectMeta: naming.InstanceConfigMap(instance)}
	instanceConfigMap.SetGroupVersionKind(v1.SchemeGroupVersion.WithKind("ConfigMap"))

	// TODO(cbandy): Instance StatefulSet as owner?
	err := errors.WithStack(r.setControllerReference(cluster, instanceConfigMap))

	instanceConfigMap.Labels = map[string]string{
		naming.LabelCluster:  cluster.Name,
		naming.LabelInstance: instance.Name,
	}

	if err == nil {
		err = patroni.InstanceConfigMap(ctx, cluster, instance, instanceConfigMap)
	}
	if err == nil {
		err = errors.WithStack(r.apply(ctx, instanceConfigMap))
	}

	return instanceConfigMap, err
}

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=secrets,verbs=create;patch

// reconcileInstanceCertificates writes the Secret that contains certificates
// and private keys for instance of cluster.
func (r *Reconciler) reconcileInstanceCertificates(
	ctx context.Context, cluster *v1beta1.PostgresCluster,
	instance *appsv1.StatefulSet, root *pki.RootCertificateAuthority,
) (*v1.Secret, error) {
	existing := &v1.Secret{ObjectMeta: naming.InstanceCertificates(instance)}
	err := errors.WithStack(client.IgnoreNotFound(
		r.Client.Get(ctx, client.ObjectKeyFromObject(existing), existing)))

	instanceCerts := &v1.Secret{ObjectMeta: naming.InstanceCertificates(instance)}
	instanceCerts.SetGroupVersionKind(v1.SchemeGroupVersion.WithKind("Secret"))

	// TODO(cbandy): Instance StatefulSet as owner?
	if err == nil {
		err = errors.WithStack(r.setControllerReference(cluster, instanceCerts))
	}

	instanceCerts.Labels = map[string]string{
		naming.LabelCluster:  cluster.Name,
		naming.LabelInstance: instance.Name,
	}

	// This secret is holding certificates, but the "kubernetes.io/tls" type
	// expects an *unencrypted* private key. We're also adding other values and
	// other formats, so indicate that with the "Opaque" type.
	// - https://docs.k8s.io/concepts/configuration/secret/#secret-types
	instanceCerts.Type = v1.SecretTypeOpaque
	instanceCerts.Data = make(map[string][]byte)

	var leafCert *pki.LeafCertificate

	if err == nil {
		leafCert, err = r.instanceCertificate(ctx, instance, existing, instanceCerts, root)
	}
	if err == nil {
		err = patroni.InstanceCertificates(ctx,
			root.Certificate, leafCert.Certificate,
			leafCert.PrivateKey, instanceCerts)
	}
	if err == nil {
		err = errors.WithStack(r.apply(ctx, instanceCerts))
	}

	return instanceCerts, err
}
