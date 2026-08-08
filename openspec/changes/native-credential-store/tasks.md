## 1. Configuration and contracts

- [x] 1.1 Add the optional provider configuration, exact allowlisted reference inventory, legacy environment behavior, and present-empty precedence in `internal/config`; prove it with `TestSecretsConfigIsOptIn`, `TestSecretReferencesAreAllowlisted`, and `TestEnvironmentPresenceWins`; commit as `feat(config): declare secret resolution`.
- [x] 1.2 Add the `internal/secretstore` provider interface, typed conditions, result metadata, portable value validation, and in-memory fake; prove it with `TestValidateValuePortableContract`, `TestProviderConditionsDoNotExposeValues`, and `TestSetGetRoundTripsPrintableCorpus`; commit as `feat(secretstore): define provider contract`.

## 2. Secure state and managed-file provider

- [x] 2.1 Add common POSIX state-directory ownership and permission validation plus restrictive artifact creation; prove it with `TestValidateStateDirPOSIX`, `TestUnsafeParentFailsBeforeArtifactRead`, and `TestRestrictedTempPrecedesSecretWrite`; commit as `feat(secretstore): secure posix state`.
- [x] 2.2 Add the POSIX managed-file snapshot, never-replaced sidecar lock, bounded non-blocking writers, lock-free readers, durable replacement, and corruption refusal; prove it with `TestFileStoreRoundTrip`, `TestFileStoreConcurrentWritersPreserveBothNames`, `TestFileStoreLockDeadline`, `TestFileStoreReaderSeesCompleteSnapshot`, and `TestFileStoreRejectsCorruptSnapshot`; commit as `feat(secretstore): add posix file provider`.

## 3. Native helper boundary

- [x] 3.1 Add the hidden one-operation helper protocol, pipe-only set input, structured redacted output, global in-process slot, and bounded cross-process `native-helper.lock`; prove it with `TestHelperProtocolRoundTrip`, `TestSetValueUsesPipeOnly`, `TestNativeSlotSerializesProcesses`, and `TestNativeBusyIsNotHealth`; commit as `feat(secretstore): isolate native operations`.
- [x] 3.2 Add POSIX private-session and process-group creation, pre-request isolation proof, durable phased markers, full helper identity checks, bounded tree termination, and wedged self-recovery; prove it with `TestPOSIXHelperIsSessionLeader`, `TestPreMarkerFailureSendsNoRequest`, `TestRecoveryNeverSignalsUnrelatedProcess`, `TestRecoveryNeverSignalsOwnSession`, `TestRequestDeadlineBoundsHelper`, `TestResponseDoesNotPermitUnboundedExit`, and `TestWedgedHelperSelfRecovers`; run the lifecycle tests with `-race -count=5`; commit as `feat(secretstore): supervise posix native helper`.

## 4. Native platform adapters

- [x] 4.1 Add the macOS no-cgo Keychain adapter with bounded default-keychain preflight, a durably recorded direct `/usr/bin/security` child in its own private-session process group, stdin-safe item operations, exact not-found mapping, interaction-required latching, and explicit retry; prove it with `TestDarwinErrorMapping`, `TestDarwinInteractionSuspendsAutomaticReads`, `TestDarwinRetryAllowsOneAttempt`, and the platform-gated `TestDarwinNativeRoundTrip`; inspect the complete helper descendant tree during set; commit as `feat(secretstore): add macos keychain provider`.
- [x] 4.2 Add the Linux Secret Service adapter and non-interactive session D-Bus, service-owner, collection, and unlock health checks; prove it with `TestLinuxHealthMapping`, `TestLinuxCreateItemReplacesAtomically`, and the platform-gated `TestLinuxNativeRoundTrip`; commit as `feat(secretstore): add linux secret service provider`.

## 5. Resolution lifecycle

- [x] 5.1 Add configuration-derived consumer groups, aggregate-bounded startup resolution, temporary-value disposal, provider-health short circuit, pending queue, contention pacing, negative backoff, and background recovery; prove it with `TestStartupResolutionIsGroupedAndBounded`, `TestPartialGroupIsDiscardedAndReResolved`, `TestWedgedProviderStartsOneHelperForManyGroups`, and `TestPendingGroupsRecover`; run concurrency tests with `-race -count=5`; commit as `feat(secretstore): coordinate consumer resolution`.
- [x] 5.2 Add the per-name presence-only cache, effective-source states, provider lookup bounds, explicit refresh, and invalidation; prove it with `TestStatusUsesConfiguredReferencesOnly`, `TestEnvironmentStatusSkipsProvider`, `TestPresenceCacheStoresNoValue`, and `TestRepeatedStatusPollsUseOneProbe`; commit as `feat(secretstore): expose redacted presence status`.
- [ ] 5.3 Route backend child environments, HTTP headers, and embeddings credentials through resolved consumer inputs; mark pending and provider-failed backend health without affecting unrelated backends; prove it with `TestBackendUsesResolvedChildEnvironment`, `TestHTTPBackendUsesResolvedHeaders`, `TestEmbeddingsUsesResolvedAPIKey`, and `TestProviderFailureIsolatesDependents`; commit as `feat: wire secret resolution into consumers`.
- [ ] 5.4 Add exact dependency indexing and targeted post-mutation reconnect after all provider locks are released; prove it with `TestSecretMutationReconnectsExactBackends`, `TestEmbeddingSecretRebuildsIndexClient`, and `TestReconnectStartsAfterMutationUnlock`; commit as `feat: reconnect secret consumers`.

## 6. Local management surfaces

- [ ] 6.1 Add `mcpd secret set`, `status`, `retry`, and `remove` with daemon-first transport, hidden prompt or EOF input, exact one-terminator removal, offline owner-identity checks, shadow warnings, and best-effort notification; prove it with `TestSecretSetNeverUsesArgument`, `TestSecretInputLineEndingRules`, `TestSecretCLIUsesDaemonFirst`, `TestOfflineSecretSetRejectsWrongOwner`, and `TestOfflineNativeSetReportsNamespace`; commit as `feat(cli): manage stored secrets`.
- [ ] 6.2 Add guarded loopback API routes for write-only set, status, refresh, retry, and remove; prove it with `TestSecretAPINeverReturnsValues`, `TestSecretAPIUsesSharedOriginGuard`, `TestSecretAPIRemoveReportsDependents`, and `TestRemoteSurfaceExcludesSecretRoutes`; commit as `feat(web): add secret management api`.
- [ ] 6.3 Add the status-page secret panel with write-only inputs, typed conditions, provider guidance, retry controls, and environment-shadow warnings using escaped templates and `textContent`; prove it with `TestSecretPanelRendersNoStoredValues`, `TestSecretPanelEscapesNamesAndErrors`, and the asset test that rejects `innerHTML`; commit as `feat(web): add secret management panel`.

## 7. External changes, documentation, and release evidence

- [ ] 7.1 Add managed-file directory watching, metadata fallback, debounce, content-digest self-write suppression, and exact-consumer refresh; prove it with `TestExternalAtomicReplacementReloadsFinalSnapshot`, `TestMetadataFallbackDetectsChange`, and `TestDaemonWriteTriggersOneReconnect`; commit as `feat(secretstore): watch managed file changes`.
- [ ] 7.2 Document provider configuration, accepted input bytes, environment precedence, native session and headless limitations, corrupt-file recovery, POSIX ownership repair, migration, and rollback; prove examples with configuration parsing tests and scan documentation for secret literals; commit as `docs: explain credential providers`.
- [ ] 7.3 Run `gofmt`, focused package tests, `go test -race -count=5` for lifecycle packages, `go test ./...`, `go vet ./...`, `govulncheck ./...`, clean `CGO_ENABLED=0` builds for supported macOS and Linux targets, platform-gated native integration tests where credential services exist, and the current macOS LaunchAgent Context7 smoke test; record exact evidence in the change and commit any test-only corrections as `test: verify credential providers`.
