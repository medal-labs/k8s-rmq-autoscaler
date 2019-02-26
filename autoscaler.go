package main

import (
	"context"
	"errors"
	"strconv"
	"time"

	"k8s.io/api/apps/v1beta1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
)

const (
	// AnnotationPrefix Prefix that will be use to find the corrects annotation on deployment
	AnnotationPrefix = "k8s-rmq-autoscaler/"
	// Enable Annotation key used to enable the scaler
	Enable = "enable"
	// Queue Annotation Key used to set rmq queue to watch
	Queue = "queue"
	// Vhost Annotation Key used to set rmq vhost where the queue can be found
	Vhost = "vhost"
	// MinWorkers Annotation Key used to set the minimum amount of worker to scale down
	MinWorkers = "min-workers"
	// MaxWorkers Annotation Key used to set the maximum amount of worker to scale up
	MaxWorkers = "max-workers"
	// Steps Annotation Key used to set how many workers will be scale up/down if needed (Default: 1)
	Steps = "steps"
	// Offset Annotation Key used to set the offset. The offset will be added if you always want more workers than message in queue.
	// For example, if you set 1 on offset, you will always have 1 worker more than messages (Default: 0)
	Offset = "offset"
	// Override Annotation Key used to set the override.
	// If override is true the user can scale more than the max/min limits manually (Default: false)
	Override = "override"
	// SafeUnscale Annotation Key used to forbid the scaler to scale down when you still have message in queue.
	// Used to avoid to unscale a worker that is processing a message (Default: true)
	SafeUnscale = "safe-unscale"
)

// Autoscaler struct that will be used to received events from discovery
type Autoscaler struct {
	add    chan *v1beta1.Deployment
	delete chan *v1beta1.Deployment
	apps   map[string]*App
	client *kubernetes.Clientset
	rmq    *rmq
}

// App struct used to store information about a deployment
type App struct {
	ref            *v1beta1.Deployment
	key            string
	queue          string
	vhost          string
	minWorkers     int32
	maxWorkers     int32
	readyWorkers   int32
	replicas       int32
	steps          int32
	offset         int32
	overrideLimits bool
	safeUnscale    bool
}

// Run launch the autoscaler scale
func (a *Autoscaler) Run(ctx context.Context, client *kubernetes.Clientset, loopTickSeconds int) {

	loopTick := time.NewTicker(time.Duration(loopTickSeconds) * time.Second)

	defer func() {
		loopTick.Stop()
	}()

	go func() {
		for {
			select {
			case deployment := <-a.add:
				key, _ := cache.MetaNamespaceKeyFunc(deployment)

				app, err := createApp(deployment, key)

				if err != nil {
					klog.Error(err)
					continue
				}

				if _, ok := a.apps[key]; ok {
					// Already exist
					klog.Infof("Updating %s app", key)
				} else {
					klog.Infof("New %s app", key)
				}

				a.apps[key] = app
			case deployment := <-a.delete:
				key, _ := cache.MetaNamespaceKeyFunc(deployment)
				klog.Infof("Deleting app %s", key)
				delete(a.apps, key)
			case <-loopTick.C:
				for key, app := range a.apps {

					consumers, queueSize, err := a.rmq.getQueueInformation(app.queue, app.vhost)

					if err != nil {
						klog.Infof("%s error during queue fetch, removing the app (%s)", app.key, err)
						continue
					}

					// Get the next scale info
					increment := app.scale(consumers, queueSize)

					if app.safeUnscale && increment < 0 && queueSize > 0 {
						klog.Infof("Safe unscale is enable in app %s, can't unscale when message are in queue", key)
					} else if increment != 0 {
						newReplica := app.replicas + increment
						klog.Infof("%s Will be updated from %d replicas to %d", key, app.replicas, newReplica)
						app.ref.Spec.Replicas = int32Ptr(newReplica)
						newRef, err := client.AppsV1beta1().Deployments(app.ref.Namespace).Update(app.ref)

						if err != nil {
							klog.Errorf("Error during deployment (%s) update, retry later (%s)", app.key, err)
						} else {
							app.ref = newRef
						}
					}
				}
			}
		}
	}()

	// Block until the target provider is explicitly canceled.
	<-ctx.Done()
}

func (app *App) scale(consumers int32, queueSize int32) int32 {
	klog.Infof("%s, starting auto-scale decision", app.key)

	if app.readyWorkers != app.replicas {
		klog.Infof("%s is currently unstable, retry later, not enough workers (ready: %d / wanted: %d)", app.key, app.readyWorkers, app.replicas)
		return 0
	}

	if consumers != app.replicas {
		klog.Infof("%s is currently unstable, consumer count not stable (ready: %d / real: %d)", app.key, app.readyWorkers, consumers)
		return 0
	}

	if consumers > app.maxWorkers {
		klog.Infof("%s have to much worker (%d), need to decrease to max (%d)", app.key, consumers, app.maxWorkers)
		if !app.overrideLimits {
			return app.maxWorkers - app.replicas
		}
		klog.Infof("%s limits are override, do nothing", app.key)
		return 0
	}

	if consumers < app.minWorkers {
		klog.Infof("%s have not enough worker (%d), need to increase to min (%d)", app.key, consumers, app.minWorkers)
		if !app.overrideLimits {
			return app.minWorkers - app.replicas
		}
		klog.Infof("%s limits are override, do nothing", app.key)
		return 0
	}

	readyMessage := queueSize - consumers + app.offset

	if readyMessage > 0 {
		if consumers == app.maxWorkers {
			klog.Infof("%s has already the maximum workers (%d), can do anything more (queueSize: %d / consumers: %d)", app.key, app.maxWorkers, queueSize, consumers)
			return 0
		}
		scaleUp := min(readyMessage, app.steps)
		klog.Infof("%s will scale with %d (steps: %d / readyMessages: %d)", app.key, scaleUp, app.steps, readyMessage)
		return scaleUp
	} else if readyMessage < 0 {
		if consumers == app.minWorkers {
			klog.Infof("%s has already the minimum workers (%d), can do anything more (queueSize: %d / consumers: %d)", app.key, app.minWorkers, queueSize, consumers)
			return 0
		}
		scaleDown := max(readyMessage, -app.steps)
		klog.Infof("%s will scale with %d (steps: %d / readyMessages: %d)", app.key, scaleDown, app.steps, readyMessage)
		return scaleDown
	}

	// Nothing to do
	klog.Infof("%s nothing to do with current queue size (queue: %d / consumers: %d / offset: %d)", app.key, queueSize, consumers, app.offset)
	return 0
}

func createApp(deployment *v1beta1.Deployment, key string) (*App, error) {
	if _, ok := deployment.ObjectMeta.Annotations[AnnotationPrefix+Enable]; !ok {
		return nil, errors.New(key + " not concerned by autoscaling, skipping")
	}

	var app *App

	if queue, ok := deployment.ObjectMeta.Annotations[AnnotationPrefix+Queue]; ok {
		app = &App{
			ref:            deployment,
			key:            key,
			queue:          queue,
			replicas:       *deployment.Spec.Replicas,
			readyWorkers:   deployment.Status.ReadyReplicas,
			overrideLimits: false,
			safeUnscale:    true,
			offset:         0,
			steps:          1,
		}
	} else {
		return nil, errors.New(key + " property `queue` not filled")
	}

	if vhost, ok := deployment.ObjectMeta.Annotations[AnnotationPrefix+Vhost]; ok {
		app.vhost = vhost
	} else {
		return nil, errors.New(key + " property `vhost` not filled")
	}

	if minWorkers, ok := deployment.ObjectMeta.Annotations[AnnotationPrefix+MinWorkers]; ok {
		minWorkers, err := strconv.ParseInt(minWorkers, 10, 32)

		if err != nil {
			return nil, err
		}

		app.minWorkers = int32(minWorkers)
	} else {
		return nil, errors.New(key + " property `min-workers` not filled")
	}

	if maxWorkers, ok := deployment.ObjectMeta.Annotations[AnnotationPrefix+MaxWorkers]; ok {
		maxWorkers, err := strconv.ParseInt(maxWorkers, 10, 32)

		if err != nil {
			return nil, err
		}

		app.maxWorkers = int32(maxWorkers)
	} else {
		return nil, errors.New(key + " property `max-workers` not filled")
	}

	if steps, ok := deployment.ObjectMeta.Annotations[AnnotationPrefix+Steps]; ok {
		steps, err := strconv.ParseInt(steps, 10, 32)

		if err != nil {
			return nil, err
		}

		app.steps = int32(steps)
	}

	if offset, ok := deployment.ObjectMeta.Annotations[AnnotationPrefix+Offset]; ok {
		offset, err := strconv.ParseInt(offset, 10, 32)

		if err != nil {
			return nil, err
		}

		app.offset = int32(offset)
	}

	if overrideLimit, ok := deployment.ObjectMeta.Annotations[AnnotationPrefix+Override]; ok {
		overrideLimit, err := strconv.ParseBool(overrideLimit)

		if err != nil {
			return nil, err
		}

		app.overrideLimits = overrideLimit
	}

	if safeUnscale, ok := deployment.ObjectMeta.Annotations[AnnotationPrefix+SafeUnscale]; ok {
		safeUnscale, err := strconv.ParseBool(safeUnscale)

		if err != nil {
			return nil, err
		}

		app.safeUnscale = safeUnscale
	}

	return app, nil
}

func int32Ptr(i int32) *int32 { return &i }

func min(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}

func max(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}