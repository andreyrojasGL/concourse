package engine_test

import (
	"context"
	"errors"
	"sync"
	"time"

	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagertest"
	"github.com/concourse/concourse/atc/db"
	"github.com/concourse/concourse/atc/db/dbfakes"
	"github.com/concourse/concourse/atc/db/lock/lockfakes"
	. "github.com/concourse/concourse/atc/engine"
	"github.com/concourse/concourse/atc/engine/enginefakes"
	"github.com/concourse/concourse/atc/exec"
	"github.com/concourse/concourse/atc/exec/execfakes"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Engine", func() {
	var (
		logger lager.Logger

		fakeBuild       *dbfakes.FakeBuild
		fakeStepBuilder *enginefakes.FakeStepBuilder
	)

	BeforeEach(func() {
		logger = lagertest.NewTestLogger("test")

		fakeBuild = new(dbfakes.FakeBuild)
		fakeBuild.IDReturns(128)

		fakeStepBuilder = new(enginefakes.FakeStepBuilder)
	})

	Describe("LookupBuild", func() {
		var (
			build Build
			err   error

			engine Engine
		)

		BeforeEach(func() {
			engine = NewEngine(fakeStepBuilder)
		})

		JustBeforeEach(func() {
			build, err = engine.LookupBuild(logger, fakeBuild)
		})

		It("succeeds", func() {
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns a build", func() {
			Expect(build).NotTo(BeNil())
		})
	})

	Describe("Build", func() {
		var (
			build     Build
			release   chan bool
			cancel    chan bool
			waitGroup *sync.WaitGroup
		)

		BeforeEach(func() {

			ctx := context.Background()
			cancel = make(chan bool)
			release = make(chan bool)
			trackedStates := new(sync.Map)
			waitGroup = new(sync.WaitGroup)

			build = NewBuild(
				ctx,
				func() { cancel <- true },
				fakeBuild,
				fakeStepBuilder,
				release,
				trackedStates,
				waitGroup,
			)
		})

		Describe("Resume", func() {
			var logger lager.Logger

			BeforeEach(func() {
				logger = lagertest.NewTestLogger("test")
			})

			JustBeforeEach(func() {
				build.Resume(logger)
			})

			Context("when acquiring the lock succeeds", func() {
				var fakeLock *lockfakes.FakeLock

				BeforeEach(func() {
					fakeLock = new(lockfakes.FakeLock)

					fakeBuild.AcquireTrackingLockReturns(fakeLock, true, nil)
				})

				Context("when the build is active", func() {
					BeforeEach(func() {
						fakeBuild.IsRunningReturns(true)
						fakeBuild.ReloadReturns(true, nil)
					})

					Context("when listening for aborts succeeds", func() {
						var abort chan struct{}
						var fakeNotifier *dbfakes.FakeNotifier

						BeforeEach(func() {
							abort = make(chan struct{})

							fakeNotifier = new(dbfakes.FakeNotifier)
							fakeNotifier.NotifyReturns(abort)

							fakeBuild.AbortNotifierReturns(fakeNotifier, nil)
						})

						Context("when converting the plan to a step succeeds", func() {
							var fakeStep *execfakes.FakeStep

							BeforeEach(func() {
								fakeStep = new(execfakes.FakeStep)

								fakeStepBuilder.BuildStepReturns(fakeStep, nil)
							})

							It("releases the lock", func() {
								waitGroup.Wait()
								Expect(fakeLock.ReleaseCallCount()).To(Equal(1))
							})

							It("closes the notifier", func() {
								waitGroup.Wait()
								Expect(fakeNotifier.CloseCallCount()).To(Equal(1))
							})

							Context("when the build is released", func() {
								BeforeEach(func() {
									readyToRelease := make(chan bool)

									go func() {
										<-readyToRelease
										release <- true
									}()

									fakeStep.RunStub = func(context.Context, exec.RunState) error {
										close(readyToRelease)
										<-time.After(time.Hour)
										return nil
									}
								})

								It("does not finish the build", func() {
									waitGroup.Wait()
									Expect(fakeBuild.FinishCallCount()).To(Equal(0))
								})
							})

							Context("when the build is aborted", func() {
								BeforeEach(func() {
									readyToAbort := make(chan bool)

									go func() {
										<-readyToAbort
										abort <- struct{}{}
									}()

									fakeStep.RunStub = func(context.Context, exec.RunState) error {
										close(readyToAbort)
										<-time.After(time.Second)
										return nil
									}
								})

								It("cancels the context", func() {
									waitGroup.Wait()
									Expect(<-cancel).To(BeTrue())
								})
							})

							Context("when the build finishes without error", func() {
								BeforeEach(func() {
									fakeStep.RunReturns(nil)
								})

								Context("when the build finishes successfully", func() {
									BeforeEach(func() {
										fakeStep.SucceededReturns(true)
									})

									It("finishes the build", func() {
										waitGroup.Wait()
										Expect(fakeBuild.FinishCallCount()).To(Equal(1))
										Expect(fakeBuild.FinishArgsForCall(0)).To(Equal(db.BuildStatusSucceeded))
									})
								})

								Context("when the build finishes woefully", func() {
									BeforeEach(func() {
										fakeStep.SucceededReturns(false)
									})

									It("finishes the build", func() {
										waitGroup.Wait()
										Expect(fakeBuild.FinishCallCount()).To(Equal(1))
										Expect(fakeBuild.FinishArgsForCall(0)).To(Equal(db.BuildStatusFailed))
									})
								})
							})

							Context("when the build finishes with error", func() {
								BeforeEach(func() {
									fakeStep.RunReturns(errors.New("nope"))
								})

								It("finishes the build", func() {
									waitGroup.Wait()
									Expect(fakeBuild.FinishCallCount()).To(Equal(1))
									Expect(fakeBuild.FinishArgsForCall(0)).To(Equal(db.BuildStatusErrored))
								})
							})

							Context("when the build finishes with cancelled error", func() {
								BeforeEach(func() {
									fakeStep.RunReturns(context.Canceled)
								})

								It("finishes the build", func() {
									waitGroup.Wait()
									Expect(fakeBuild.FinishCallCount()).To(Equal(1))
									Expect(fakeBuild.FinishArgsForCall(0)).To(Equal(db.BuildStatusAborted))
								})
							})
						})

						Context("when converting the plan to a step fails", func() {
							BeforeEach(func() {
								fakeStepBuilder.BuildStepReturns(nil, errors.New("nope"))
							})

							It("releases the lock", func() {
								Expect(fakeLock.ReleaseCallCount()).To(Equal(1))
							})

							It("closes the notifier", func() {
								Expect(fakeNotifier.CloseCallCount()).To(Equal(1))
							})
						})
					})

					Context("when listening for aborts fails", func() {
						BeforeEach(func() {
							fakeBuild.AbortNotifierReturns(nil, errors.New("nope"))
						})

						It("does not build the step", func() {
							Expect(fakeStepBuilder.BuildStepCallCount()).To(BeZero())
						})

						It("releases the lock", func() {
							Expect(fakeLock.ReleaseCallCount()).To(Equal(1))
						})
					})
				})

				Context("when the build is not yet active", func() {
					BeforeEach(func() {
						fakeBuild.ReloadReturns(true, nil)
					})

					It("does not build the step", func() {
						Expect(fakeStepBuilder.BuildStepCallCount()).To(BeZero())
					})

					It("releases the lock", func() {
						Expect(fakeLock.ReleaseCallCount()).To(Equal(1))
					})
				})

				Context("when the build has already finished", func() {
					BeforeEach(func() {
						fakeBuild.ReloadReturns(true, nil)
						fakeBuild.StatusReturns(db.BuildStatusSucceeded)
					})

					It("does not build the step", func() {
						Expect(fakeStepBuilder.BuildStepCallCount()).To(BeZero())
					})

					It("releases the lock", func() {
						Expect(fakeLock.ReleaseCallCount()).To(Equal(1))
					})
				})

				Context("when the build is no longer in the database", func() {
					BeforeEach(func() {
						fakeBuild.ReloadReturns(false, nil)
					})

					It("does not build the step", func() {
						Expect(fakeStepBuilder.BuildStepCallCount()).To(BeZero())
					})

					It("releases the lock", func() {
						Expect(fakeLock.ReleaseCallCount()).To(Equal(1))
					})
				})
			})

			Context("when acquiring the lock fails", func() {
				BeforeEach(func() {
					fakeBuild.AcquireTrackingLockReturns(nil, false, errors.New("no lock for you"))
				})

				It("does not build the step", func() {
					Expect(fakeStepBuilder.BuildStepCallCount()).To(BeZero())
				})
			})
		})
	})
})
