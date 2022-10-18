package clusterops

import (
	"context"
	"crypto/md5"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/kubean-io/kubean/pkg/util/entrypoint"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"kubean.io/api/apis"
	clusterv1alpha1 "kubean.io/api/apis/cluster/v1alpha1"
	clusteroperationv1alpha1 "kubean.io/api/apis/clusteroperation/v1alpha1"
	"kubean.io/api/constants"
	clusterClientSet "kubean.io/api/generated/cluster/clientset/versioned"
	clusterOperationClientSet "kubean.io/api/generated/clusteroperation/clientset/versioned"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	RequeueAfter     = time.Millisecond * 500
	LoopForJobStatus = time.Second * 3
)

type Controller struct {
	client.Client
	ClientSet           kubernetes.Interface
	KubeanClusterSet    clusterClientSet.Interface
	KubeanClusterOpsSet clusterOperationClientSet.Interface
}

func (c *Controller) Start(ctx context.Context) error {
	klog.Warningf("ClusterOperation Controller Start")
	<-ctx.Done()
	return nil
}

const BaseSlat = "kubean"

func (c *Controller) CalSalt(clusterOps *clusteroperationv1alpha1.ClusterOperation) string {
	summaryStr := ""
	summaryStr += BaseSlat
	summaryStr += clusterOps.Spec.Cluster
	summaryStr += string(clusterOps.Spec.ActionType)
	summaryStr += strings.TrimSpace(clusterOps.Spec.Action)
	summaryStr += strconv.Itoa(clusterOps.Spec.BackoffLimit)
	summaryStr += clusterOps.Spec.Image
	for _, action := range clusterOps.Spec.PreHook {
		summaryStr += string(action.ActionType)
		summaryStr += strings.TrimSpace(action.Action)
	}
	for _, action := range clusterOps.Spec.PostHook {
		summaryStr += string(action.ActionType)
		summaryStr += strings.TrimSpace(action.Action)
	}
	return fmt.Sprintf("%x", md5.Sum([]byte(summaryStr)))
}

func (c *Controller) UpdateClusterOpsStatusDigest(clusterOps *clusteroperationv1alpha1.ClusterOperation) (bool, error) {
	if len(clusterOps.Status.Digest) != 0 {
		// already has value.
		return false, nil
	}
	// init salt value.
	clusterOps.Status.Digest = c.CalSalt(clusterOps)
	if err := c.Status().Update(context.Background(), clusterOps); err != nil {
		return false, err
	}
	return true, nil
}

func (c *Controller) compareDigest(clusterOps *clusteroperationv1alpha1.ClusterOperation) bool {
	return clusterOps.Status.Digest == c.CalSalt(clusterOps)
}

func (c *Controller) UpdateStatusHasModified(clusterOps *clusteroperationv1alpha1.ClusterOperation) (bool, error) {
	if len(clusterOps.Status.Digest) == 0 {
		return false, nil
	}
	if clusterOps.Status.HasModified {
		// already true.
		return false, nil
	}
	if same := c.compareDigest(clusterOps); !same {
		// compare
		clusterOps.Status.HasModified = true
		if err := c.Status().Update(context.Background(), clusterOps); err != nil {
			return false, err
		}
		klog.Warningf("clusterOps %s Spec has been modified", clusterOps.Name)
		return true, nil
	}
	return false, nil
}

func (c *Controller) UpdateStatusLoop(clusterOps *clusteroperationv1alpha1.ClusterOperation, fetchJobStatus func(*clusteroperationv1alpha1.ClusterOperation) (clusteroperationv1alpha1.OpsStatus, error)) (bool, error) {
	if clusterOps.Status.Status == clusteroperationv1alpha1.RunningStatus || len(clusterOps.Status.Status) == 0 {
		// need fetch jobStatus again when the last status of job is running
		jobStatus, err := fetchJobStatus(clusterOps)
		if err != nil {
			return false, err
		}
		if jobStatus == clusteroperationv1alpha1.RunningStatus {
			// still running
			return true, nil // requeue for loop ask for status
		}
		// the status  succeed or failed
		clusterOps.Status.Status = jobStatus
		clusterOps.Status.EndTime = &metav1.Time{Time: time.Now()}
		if err := c.Client.Status().Update(context.Background(), clusterOps); err != nil {
			return false, err
		}
		return false, nil // need not requeue because the job is finished.
	}
	// already finished(succeed or failed)
	return false, nil
}

func (c *Controller) FetchJobStatus(clusterOps *clusteroperationv1alpha1.ClusterOperation) (clusteroperationv1alpha1.OpsStatus, error) {
	if clusterOps.Status.JobRef.IsEmpty() {
		return "", fmt.Errorf("clusterOps %s no job", clusterOps.Name)
	}
	targetJob, err := c.ClientSet.BatchV1().Jobs(clusterOps.Status.JobRef.NameSpace).Get(context.Background(), clusterOps.Status.JobRef.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// maybe the job is removed.
		klog.Errorf("clusterOps %s  job %s not found", clusterOps.Name, clusterOps.Status.JobRef.Name)
		return clusteroperationv1alpha1.FailedStatus, nil
	}
	if err != nil {
		return "", err
	}
	if targetJob.Status.Failed > 0 {
		return clusteroperationv1alpha1.FailedStatus, nil
	}
	if targetJob.Status.Succeeded > 0 {
		return clusteroperationv1alpha1.SucceededStatus, nil
	}
	return clusteroperationv1alpha1.RunningStatus, nil
}

func (c *Controller) ListClusterOps(clusterName string) ([]clusteroperationv1alpha1.ClusterOperation, error) {
	list, err := c.KubeanClusterOpsSet.KubeanV1alpha1().ClusterOperations().List(context.Background(), metav1.ListOptions{LabelSelector: labels.SelectorFromSet(map[string]string{constants.KubeanClusterLabelKey: clusterName}).String()})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (c *Controller) CurrentJobNeedBlock(clusterOps *clusteroperationv1alpha1.ClusterOperation, listClusterOps func(clusterName string) ([]clusteroperationv1alpha1.ClusterOperation, error)) (bool, error) {
	clusterOpsList, err := listClusterOps(clusterOps.Spec.Cluster)
	if err != nil {
		return false, err
	}
	filter := func(ops clusteroperationv1alpha1.ClusterOperation) bool {
		// todo: clusterOps has the group label and number label, first find the early group and then find the before number in the same group if possible
		// try to find the early running clusterOps job in the same cluster
		return ops.Name != clusterOps.Name &&
			ops.CreationTimestamp.UnixMilli() < clusterOps.CreationTimestamp.UnixMilli() && // <= or < ? , use "<" to avoid two jobs with the same createTime waiting for each others(blocked by each others) ,createTimes is base on second not mills.
			(ops.Status.Status == clusteroperationv1alpha1.RunningStatus || ops.Status.JobRef.IsEmpty()) // Empty jobRef means the job is blocked or ready to run.
	}
	runningClusterOpsList := make([]clusteroperationv1alpha1.ClusterOperation, 0)
	for i := range clusterOpsList {
		if filter(clusterOpsList[i]) {
			runningClusterOpsList = append(runningClusterOpsList, clusterOpsList[i])
		}
	}
	return len(runningClusterOpsList) != 0, nil
}

func IsValidImageName(image string) bool {
	isNumberOrLetter := func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsNumber(r)
	}
	if len(image) == 0 || strings.Contains(image, " ") {
		return false
	}
	runeSlice := []rune(image)
	return isNumberOrLetter(runeSlice[0]) && isNumberOrLetter(runeSlice[len(runeSlice)-1])
}

func (c *Controller) Reconcile(ctx context.Context, req controllerruntime.Request) (controllerruntime.Result, error) {
	clusterOps := &clusteroperationv1alpha1.ClusterOperation{}
	if err := c.Client.Get(ctx, req.NamespacedName, clusterOps); err != nil {
		if apierrors.IsNotFound(err) {
			return controllerruntime.Result{Requeue: false}, nil
		}
		klog.Error(err)
		return controllerruntime.Result{RequeueAfter: RequeueAfter}, err
	}

	if clusterOps.Status.Status == clusteroperationv1alpha1.FailedStatus || clusterOps.Status.Status == clusteroperationv1alpha1.SucceededStatus {
		// return early
		return controllerruntime.Result{Requeue: false}, nil
	}

	cluster, err := c.GetKuBeanCluster(clusterOps)
	if err != nil {
		klog.Error(err)
		return controllerruntime.Result{RequeueAfter: RequeueAfter}, err
	}

	if !IsValidImageName(clusterOps.Spec.Image) {
		klog.Errorf("clusterOps %s has wrong image format and update status Failed", clusterOps.Name)
		clusterOps.Status.Status = clusteroperationv1alpha1.FailedStatus
		if err := c.Client.Status().Update(ctx, clusterOps); err != nil {
			klog.Error(err)
		}
		return controllerruntime.Result{Requeue: false}, nil
	}

	if err := c.CheckClusterDataRef(cluster, clusterOps); err != nil {
		klog.Error(err.Error())
		clusterOps.Status.Status = clusteroperationv1alpha1.FailedStatus
		if err := c.Client.Status().Update(ctx, clusterOps); err != nil {
			klog.Error(err)
		}
		return controllerruntime.Result{Requeue: false}, nil
	}

	needRequeue, err := c.UpdateClusterOpsStatusDigest(clusterOps)
	if err != nil {
		klog.Error(err)
		return controllerruntime.Result{RequeueAfter: RequeueAfter}, err
	}
	if needRequeue {
		return controllerruntime.Result{RequeueAfter: RequeueAfter}, nil
	}
	needRequeue, err = c.UpdateStatusHasModified(clusterOps)
	if err != nil {
		return controllerruntime.Result{RequeueAfter: RequeueAfter}, err
	}
	if needRequeue {
		return controllerruntime.Result{RequeueAfter: RequeueAfter}, nil
	}
	needRequeue, err = c.BackUpDataRef(clusterOps, cluster)
	if err != nil {
		klog.Error(err)
		return controllerruntime.Result{RequeueAfter: RequeueAfter}, err
	}
	if needRequeue {
		// something(spec) updated ,so continue the next loop.
		return controllerruntime.Result{RequeueAfter: RequeueAfter}, nil
	}

	needRequeue, err = c.CreateEntryPointShellConfigMap(clusterOps)
	if argsErr, ok := err.(entrypoint.ArgsError); ok {
		// preHook or postHook or action error args
		klog.Errorf("clusterOps %s wrong args %s and update status Failed", clusterOps.Name, argsErr.Error())
		clusterOps.Status.Status = clusteroperationv1alpha1.FailedStatus
		if err := c.Client.Status().Update(ctx, clusterOps); err != nil {
			klog.Error(err)
		}
		return controllerruntime.Result{Requeue: false}, err
	}
	if err != nil {
		klog.Error(err)
		return controllerruntime.Result{RequeueAfter: RequeueAfter}, err
	}
	if needRequeue {
		// something updated.
		return controllerruntime.Result{RequeueAfter: RequeueAfter}, nil
	}

	needBlock, err := c.CurrentJobNeedBlock(clusterOps, c.ListClusterOps)
	if err != nil {
		klog.Error(err)
		return controllerruntime.Result{RequeueAfter: RequeueAfter}, err
	}
	if needBlock {
		klog.Infof("clusterOps %s is blocked and waiting for other clusterOps completed", clusterOps.Name)
		if clusterOps.Status.Status != clusteroperationv1alpha1.BlockedStatus {
			clusterOps.Status.Status = clusteroperationv1alpha1.BlockedStatus
			if err := c.Status().Update(context.Background(), clusterOps); err != nil {
				klog.Warningf("clusterOps %s update Status to Blocked but %s", clusterOps.Name, err.Error())
				return controllerruntime.Result{RequeueAfter: RequeueAfter}, err
			}
		}
		return controllerruntime.Result{RequeueAfter: LoopForJobStatus}, nil
	}

	needRequeue, err = c.CreateKubeSprayJob(clusterOps)
	if err != nil {
		klog.Error(err)
		return controllerruntime.Result{RequeueAfter: RequeueAfter}, err
	}
	if needRequeue {
		return controllerruntime.Result{RequeueAfter: RequeueAfter}, nil
	}

	needRequeue, err = c.UpdateStatusLoop(clusterOps, c.FetchJobStatus)
	if err != nil {
		klog.Error(err)
		return controllerruntime.Result{RequeueAfter: RequeueAfter}, err
	}
	if needRequeue {
		return controllerruntime.Result{RequeueAfter: LoopForJobStatus}, nil
	}
	return controllerruntime.Result{Requeue: false}, nil
}

func (c *Controller) NewKubesprayJob(clusterOps *clusteroperationv1alpha1.ClusterOperation) *batchv1.Job {
	BackoffLimit := int32(clusterOps.Spec.BackoffLimit)
	DefaultMode := int32(0o700)
	PrivatekeyMode := int32(0o400)
	jobName := c.GenerateJobName(clusterOps)
	namespace := clusterOps.Spec.HostsConfRef.NameSpace
	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "batch/v1",
			Kind:       "Job",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      jobName,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &BackoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: "kubean",
					Containers: []corev1.Container{
						{
							Name:    "kubespray", // do not change this name
							Image:   clusterOps.Spec.Image,
							Command: []string{"/bin/entrypoint.sh"},
							Env: []corev1.EnvVar{
								{
									Name:  "CLUSTER_NAME",
									Value: clusterOps.Spec.Cluster,
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "entrypoint",
									MountPath: "/bin/entrypoint.sh",
									SubPath:   "entrypoint.sh",
									ReadOnly:  true,
								},
								{
									Name:      "hosts-conf",
									MountPath: "/conf/hosts.yml",
									SubPath:   "hosts.yml",
								},
								{
									Name:      "vars-conf",
									MountPath: "/conf/group_vars.yml",
									SubPath:   "group_vars.yml",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "entrypoint",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: clusterOps.Spec.EntrypointSHRef.Name,
									},
									DefaultMode: &DefaultMode,
								},
							},
						},
						{
							Name: "hosts-conf",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: clusterOps.Spec.HostsConfRef.Name,
									},
								},
							},
						},
						{
							Name: "vars-conf",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: clusterOps.Spec.VarsConfRef.Name,
									},
								},
							},
						},
					},
				},
			},
		},
	}
	if !clusterOps.Spec.SSHAuthRef.IsEmpty() {
		// mount ssh data
		if len(job.Spec.Template.Spec.Containers) > 0 && job.Spec.Template.Spec.Containers[0].Name == "kubespray" {
			job.Spec.Template.Spec.Containers[0].VolumeMounts = append(job.Spec.Template.Spec.Containers[0].VolumeMounts,
				corev1.VolumeMount{
					Name:      "ssh-auth",
					MountPath: "/auth/ssh-privatekey",
					SubPath:   "ssh-privatekey",
					ReadOnly:  true,
				})
		}
		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes,
			corev1.Volume{
				Name: "ssh-auth",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName:  clusterOps.Spec.SSHAuthRef.Name,
						DefaultMode: &PrivatekeyMode, // fix Permissions 0644 are too open
					},
				},
			})
	}
	return job
}

func (c *Controller) GenerateJobName(clusterOps *clusteroperationv1alpha1.ClusterOperation) string {
	return fmt.Sprintf("kubean-%s-job", clusterOps.Name)
}

func (c *Controller) CreateKubeSprayJob(clusterOps *clusteroperationv1alpha1.ClusterOperation) (bool, error) {
	if !clusterOps.Status.JobRef.IsEmpty() {
		return false, nil
	}
	jobName := c.GenerateJobName(clusterOps)
	namespace := clusterOps.Spec.HostsConfRef.NameSpace
	job, err := c.ClientSet.BatchV1().Jobs(namespace).Get(context.Background(), jobName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// the job doest not exist , and will create the job.
			klog.Warningf("create job %s for kuBeanClusterOp %s", jobName, clusterOps.Name)
			job = c.NewKubesprayJob(clusterOps)

			c.SetOwnerReferences(&job.ObjectMeta, clusterOps)
			job, err = c.ClientSet.BatchV1().Jobs(job.Namespace).Create(context.Background(), job, metav1.CreateOptions{})
			if err != nil {
				return false, err
			}
		} else {
			// other error.
			klog.Error(err)
			return false, err
		}
	}
	clusterOps.Status.JobRef = &apis.JobRef{
		NameSpace: job.Namespace,
		Name:      job.Name,
	}
	clusterOps.Status.StartTime = &metav1.Time{Time: time.Now()}
	clusterOps.Status.Status = clusteroperationv1alpha1.RunningStatus
	clusterOps.Status.Action = clusterOps.Spec.Action

	if err := c.Status().Update(context.Background(), clusterOps); err != nil {
		return false, err
	}
	return true, nil
}

// GetKuBeanCluster fetch the cluster which clusterOps belongs to.
func (c *Controller) GetKuBeanCluster(clusterOps *clusteroperationv1alpha1.ClusterOperation) (*clusterv1alpha1.Cluster, error) {
	// cluster has many clusterOps.
	return c.KubeanClusterSet.KubeanV1alpha1().Clusters().Get(context.Background(), clusterOps.Spec.Cluster, metav1.GetOptions{})
}

// CreateEntryPointShellConfigMap create configMap to store entrypoint.sh.
func (c *Controller) CreateEntryPointShellConfigMap(clusterOps *clusteroperationv1alpha1.ClusterOperation) (bool, error) {
	if !clusterOps.Spec.EntrypointSHRef.IsEmpty() {
		return false, nil
	}
	entryPointData := entrypoint.NewEntryPoint()
	isPrivateKey := !clusterOps.Spec.SSHAuthRef.IsEmpty()
	for _, action := range clusterOps.Spec.PreHook {
		if err := entryPointData.PreHookRunPart(string(action.ActionType), action.Action, action.ExtraArgs, isPrivateKey); err != nil {
			return false, err
		}
	}
	if err := entryPointData.SprayRunPart(string(clusterOps.Spec.ActionType), clusterOps.Spec.Action, clusterOps.Spec.ExtraArgs, isPrivateKey); err != nil {
		return false, err
	}
	for _, action := range clusterOps.Spec.PostHook {
		if err := entryPointData.PostHookRunPart(string(action.ActionType), action.Action, action.ExtraArgs, isPrivateKey); err != nil {
			return false, err
		}
	}
	configMapData, err := entryPointData.Render()
	if err != nil {
		return false, err
	}

	newConfigMap := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-entrypoint", clusterOps.Name),
			Namespace: clusterOps.Spec.HostsConfRef.NameSpace,
		},
		Data: map[string]string{"entrypoint.sh": strings.TrimSpace(configMapData)}, // |2+
	}
	c.SetOwnerReferences(&newConfigMap.ObjectMeta, clusterOps)
	if newConfigMap, err = c.ClientSet.CoreV1().ConfigMaps(newConfigMap.Namespace).Create(context.Background(), newConfigMap, metav1.CreateOptions{}); err != nil {
		return false, err
	}
	clusterOps.Spec.EntrypointSHRef = &apis.ConfigMapRef{
		NameSpace: newConfigMap.Namespace,
		Name:      newConfigMap.Name,
	}
	if err := c.Client.Update(context.Background(), clusterOps); err != nil {
		return false, err
	}
	return true, nil
}

func (c *Controller) SetOwnerReferences(objectMetaData *metav1.ObjectMeta, clusterOps *clusteroperationv1alpha1.ClusterOperation) {
	objectMetaData.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(clusterOps, clusteroperationv1alpha1.SchemeGroupVersion.WithKind("ClusterOperation"))}
}

func (c *Controller) CopyConfigMap(clusterOps *clusteroperationv1alpha1.ClusterOperation, oldConfigMapRef *apis.ConfigMapRef, newName string) (*corev1.ConfigMap, error) {
	oldConfigMap, err := c.ClientSet.CoreV1().ConfigMaps(oldConfigMapRef.NameSpace).Get(context.Background(), oldConfigMapRef.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	newConfigMap := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      newName,
			Namespace: oldConfigMapRef.NameSpace,
		},
		Data: oldConfigMap.Data,
	}
	c.SetOwnerReferences(&newConfigMap.ObjectMeta, clusterOps)
	newConfigMap, err = c.ClientSet.CoreV1().ConfigMaps(newConfigMap.Namespace).Create(context.Background(), newConfigMap, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	return newConfigMap, nil
}

func (c *Controller) CopySecret(clusterOps *clusteroperationv1alpha1.ClusterOperation, oldSecretRef *apis.SecretRef, newName string) (*corev1.Secret, error) {
	oldSecret, err := c.ClientSet.CoreV1().Secrets(oldSecretRef.NameSpace).Get(context.Background(), oldSecretRef.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	newSecret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Secret",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      newName,
			Namespace: oldSecretRef.NameSpace,
		},
		Data: oldSecret.Data,
	}
	c.SetOwnerReferences(&newSecret.ObjectMeta, clusterOps)
	newSecret, err = c.ClientSet.CoreV1().Secrets(newSecret.Namespace).Create(context.Background(), newSecret, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	return newSecret, nil
}

// BackUpDataRef perform the backup of configRef and secretRef and return (needRequeue,error).
func (c *Controller) BackUpDataRef(clusterOps *clusteroperationv1alpha1.ClusterOperation, cluster *clusterv1alpha1.Cluster) (bool, error) {
	timestamp := fmt.Sprintf("-%d", time.Now().UnixMilli())
	if cluster.Spec.HostsConfRef.IsEmpty() || cluster.Spec.VarsConfRef.IsEmpty() {
		// cluster.Spec.SSHAuthRef.IsEmpty()
		return false, fmt.Errorf("cluster %s DataRef has empty value", cluster.Name)
	}
	if clusterOps.Labels == nil {
		clusterOps.Labels = map[string]string{constants.KubeanClusterLabelKey: cluster.Name}
	} else {
		clusterOps.Labels[constants.KubeanClusterLabelKey] = cluster.Name
	}
	if clusterOps.Spec.HostsConfRef.IsEmpty() {
		newConfigMap, err := c.CopyConfigMap(clusterOps, cluster.Spec.HostsConfRef, cluster.Spec.HostsConfRef.Name+timestamp)
		if err != nil {
			return false, err
		}
		clusterOps.Spec.HostsConfRef = &apis.ConfigMapRef{
			NameSpace: newConfigMap.Namespace,
			Name:      newConfigMap.Name,
		}
		if err := c.Client.Update(context.Background(), clusterOps); err != nil {
			return false, err
		}
		return true, nil
	}
	if clusterOps.Spec.VarsConfRef.IsEmpty() {
		newConfigMap, err := c.CopyConfigMap(clusterOps, cluster.Spec.VarsConfRef, cluster.Spec.VarsConfRef.Name+timestamp)
		if err != nil {
			return false, err
		}
		clusterOps.Spec.VarsConfRef = &apis.ConfigMapRef{
			NameSpace: newConfigMap.Namespace,
			Name:      newConfigMap.Name,
		}
		if err := c.Client.Update(context.Background(), clusterOps); err != nil {
			return false, err
		}
		return true, nil
	}
	if clusterOps.Spec.SSHAuthRef.IsEmpty() && !cluster.Spec.SSHAuthRef.IsEmpty() {
		// clusterOps backups ssh data when cluster has ssh data.
		newSecret, err := c.CopySecret(clusterOps, cluster.Spec.SSHAuthRef, cluster.Spec.SSHAuthRef.Name+timestamp)
		if err != nil {
			return false, err
		}
		clusterOps.Spec.SSHAuthRef = &apis.SecretRef{
			NameSpace: newSecret.Namespace,
			Name:      newSecret.Name,
		}
		if err := c.Client.Update(context.Background(), clusterOps); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil // needRequeue,err
}

func (c *Controller) SetupWithManager(mgr controllerruntime.Manager) error {
	return utilerrors.NewAggregate([]error{
		controllerruntime.NewControllerManagedBy(mgr).For(&clusteroperationv1alpha1.ClusterOperation{}).Complete(c),
		mgr.Add(c),
	})
}

func (c *Controller) CheckConfigMapExist(namespace, name string) bool {
	if _, err := c.ClientSet.CoreV1().ConfigMaps(namespace).Get(context.Background(), name, metav1.GetOptions{}); err != nil && apierrors.IsNotFound(err) {
		return false
	}
	return true
}

func (c *Controller) CheckSecretExist(namespace, name string) bool {
	if _, err := c.ClientSet.CoreV1().Secrets(namespace).Get(context.Background(), name, metav1.GetOptions{}); err != nil && apierrors.IsNotFound(err) {
		return false
	}
	return true
}

func (c *Controller) CheckClusterDataRef(cluster *clusterv1alpha1.Cluster, clusterOPS *clusteroperationv1alpha1.ClusterOperation) error {
	namespaceSet := map[string]struct{}{}
	if clusterOPS.Spec.HostsConfRef.IsEmpty() {
		// check HostsConfRef in cluster before clusterSpec is not assigned backup data.
		hostsConfRef := cluster.Spec.HostsConfRef
		if hostsConfRef.IsEmpty() {
			return fmt.Errorf("kubeanCluster %s hostsConfRef is empty", cluster.Name)
		}
		if !c.CheckConfigMapExist(hostsConfRef.NameSpace, hostsConfRef.Name) {
			return fmt.Errorf("kubeanCluster %s hostsConfRef %s,%s not found", cluster.Name, hostsConfRef.NameSpace, hostsConfRef.Name)
		}
		namespaceSet[hostsConfRef.NameSpace] = struct{}{}
	}
	if clusterOPS.Spec.VarsConfRef.IsEmpty() {
		varsConfRef := cluster.Spec.VarsConfRef
		if varsConfRef.IsEmpty() {
			return fmt.Errorf("kubeanCluster %s varsConfRef is empty", cluster.Name)
		}
		if !c.CheckConfigMapExist(varsConfRef.NameSpace, varsConfRef.Name) {
			return fmt.Errorf("kubeanCluster %s varsConfRef %s,%s not found", cluster.Name, varsConfRef.NameSpace, varsConfRef.Name)
		}
		namespaceSet[varsConfRef.NameSpace] = struct{}{}
	}
	if clusterOPS.Spec.SSHAuthRef.IsEmpty() && !cluster.Spec.SSHAuthRef.IsEmpty() {
		// check SSHAuthRef optionally.
		sshAuthRef := cluster.Spec.SSHAuthRef
		if !c.CheckSecretExist(sshAuthRef.NameSpace, sshAuthRef.Name) {
			return fmt.Errorf("kubeanCluster %s sshAuthRef %s,%s not found", cluster.Name, sshAuthRef.NameSpace, sshAuthRef.Name)
		}
		namespaceSet[sshAuthRef.NameSpace] = struct{}{}
	}
	if len(namespaceSet) > 1 {
		return fmt.Errorf("kubeanCluster %s hostsConfRef varsConfRef or sshAuthRef not in the same namespace", cluster.Name)
	}
	return nil
}
