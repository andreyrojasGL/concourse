package lidar

import (
	"context"
	"errors"
	"fmt"

	"code.cloudfoundry.org/lager"
	"github.com/concourse/concourse/atc/creds"
	"github.com/concourse/concourse/atc/db"
	"github.com/concourse/concourse/atc/resource"
	"github.com/concourse/concourse/atc/worker"
)

var ErrFailedToAcquireLock = errors.New("failed to acquire lock")

func NewChecker(
	logger lager.Logger,
	resourceFactory db.ResourceFactory,
	secrets creds.Secrets,
	externalURL string,
) *checker {
	return &checker{
		logger,
		resourceFactory,
		secrets,
		externalURL,
	}
}

type checker struct {
	logger          lager.Logger
	resourceFactory db.ResourceFactory
	secrets         creds.Secrets
	externalURL     string
}

func (c *checker) Run(ctx context.Context) error {

	resourceChecks, err := c.resourceFactory.ResourceChecks()
	if err != nil {
		c.logger.Error("failed-to-fetch-resource-checks", err)
		return err
	}

	for _, resourceCheck := range resourceChecks {
		go c.check(ctx, resourceCheck)
	}

	return nil
}

func (c *checker) check(ctx context.Context, resourceCheck db.ResourceCheck) error {

	if err := c.tryCheck(ctx, resourceCheck); err != nil {
		if err == ErrFailedToAcquireLock {
			return err
		}

		if err = resourceCheck.FinishWithError(err.Error()); err != nil {
			c.logger.Error("failed-to-udpate-resource-check-error", err)
			return err
		}
	}

	return nil
}

func (c *checker) tryCheck(ctx context.Context, resourceCheck db.ResourceCheck) error {

	resource, err := resourceCheck.Resource()
	if err != nil {
		c.logger.Error("failed-to-fetch-resource", err)
		return err
	}

	// This could have changed based on new variable interpolation so update it
	resourceConfigScope, err := resource.UpdateResourceConfigScope(c.secrets)
	if err != nil {
		c.logger.Error("failed-to-update-resource-config", err)
		return err
	}

	logger := c.logger.Session("check", lager.Data{
		"resource_id":        resource.ID(),
		"resource_name":      resource.Name(),
		"resource_config_id": resourceConfigScope.ResourceConfig().ID(),
	})

	for {
		lock, acquired, err := resourceConfigScope.AcquireResourceCheckingLock(logger)
		if err != nil {
			logger.Error("failed-to-get-lock", err)
			return ErrFailedToAcquireLock
		}

		if !acquired {
			logger.Debug("lock-not-acquired")
			return ErrFailedToAcquireLock
		}

		defer lock.Release()
	}

	if err = resourceCheck.Start(); err != nil {
		logger.Error("failed-to-start-resource-check", err)
		return err
	}

	parent, err := resource.ParentResourceType()
	if err != nil {
		logger.Error("failed-to-fetch-parent-type", err)
		return err
	}

	if parent.Version() == nil {
		if _, err = c.resourceFactory.CreateResourceCheck(parent.ID(), db.CheckTypeResourceType); err != nil {
			logger.Error("failed-to-request-parent-check", err)
			return err
		}

		return errors.New("parent resource has no version")
	}

	res, err := c.createResource(logger, ctx, resource)
	if err != nil {
		logger.Error("failed-to-create-resource-checker", err)
		return err
	}

	deadline, cancel := context.WithTimeout(ctx, resourceCheck.Timeout())
	defer cancel()

	logger.Debug("checking", lager.Data{"from": resourceCheck.FromVersion()})

	versions, err := res.Check(deadline, resourceConfigScope.Source(), resourceCheck.FromVersion())
	if err != nil {
		if err == context.DeadlineExceeded {
			return fmt.Errorf("Timed out after %v while checking for new versions", resourceCheck.Timeout())
		}
		return err
	}

	if err = resourceConfigScope.SaveVersions(versions); err != nil {
		return err
	}

	return resourceCheck.Finish()
}

func (c *checker) createResource(logger lager.Logger, ctx context.Context, dbResource db.Resource) (resource.Resource, error) {

	pipeline, err := dbResource.Pipeline()
	if err != nil {
		return nil, err
	}

	resourceTypes, err := pipeline.ResourceTypes()
	if err != nil {
		return nil, err
	}

	metadata := resource.TrackerMetadata{
		ResourceName: dbResource.Name(),
		PipelineName: dbResource.PipelineName(),
		ExternalURL:  c.externalURL,
	}

	containerSpec := worker.ContainerSpec{
		ImageSpec: worker.ImageSpec{
			ResourceType: dbResource.Type(),
		},
		BindMounts: []worker.BindMountSource{
			&worker.CertsVolumeMount{Logger: c.logger},
		},
		Tags:   dbResource.Tags(),
		TeamID: dbResource.TeamID(),
		Env:    metadata.Env(),
	}

	workerSpec := worker.WorkerSpec{
		ResourceType:  dbResource.Type(),
		Tags:          dbResource.Tags(),
		ResourceTypes: resourceTypes,
		TeamID:        dbResource.TeamID(),
	}

	owner := db.NewResourceConfigCheckSessionContainerOwner(
		dbResource.Config(),
		ContainerExpiries,
	)

	containerMetadata := db.ContainerMetadata{
		Type: db.ContainerTypeCheck,
	}

	chosenWorker, err := c.pool.FindOrChooseWorkerForContainer(
		logger,
		owner,
		containerSpec,
		workerSpec,
		worker.NewRandomPlacementStrategy(),
	)
	if err != nil {
		logger.Error("failed-to-choose-a-worker", err)
		return nil, err
	}

	container, err := chosenWorker.FindOrCreateContainer(
		context.Background(),
		logger,
		worker.NoopImageFetchingDelegate{},
		owner,
		containerMetadata,
		containerSpec,
		resourceTypes,
	)
	if err != nil {
		logger.Error("failed-to-create-or-find-container", err)
		return nil, err
	}

	return resource.NewResource(container), nil
}
