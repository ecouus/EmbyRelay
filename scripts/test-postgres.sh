#!/usr/bin/env bash
# The full SQLite suite already covers HTTP/protocol behavior. Recheck database
# semantics on PostgreSQL; use --full for changes to SQL/dialect/schema handling.
set -euo pipefail
cd "$(dirname "$0")/.."

mode=${1:---focused}
case "$mode" in
  --focused|--full|--list) ;;
  *) echo 'Usage: bash scripts/test-postgres.sh [--focused|--full|--list]' >&2; exit 2 ;;
esac

# Exact names keep additions deliberate and prevent broad prefixes from silently
# pulling the entire controller suite back into the second validation stage.
tests=(
  TestDatabaseCopyRoundTripAndResume
  TestDeletedRouteDetachesAccountAndPreservesUsage
  TestUsageRetentionSchemaUpgrade
  TestHistoricalLoginCollisionStopsMigration
  TestGlobalLoginNames
  TestManagementPersistenceAndAuth
  TestChangeOwnPasswordRevokesExistingSession
  TestResetAdminPassword
  TestRecoverableKeyAndSnapshot
  TestPortalSharedAccountIsolation
  TestAdminUserSourcesPrivacyOwnershipAndAddresses
  TestPortalValidationFailurePreservesSource
  TestDevicePolicyOverridesAndAccessControl
  TestRemoteDiagnosticValidationAndRetention
  TestQuotaAtomicAndDurable
  TestDistributedQuotaReservations
  TestHistoricalTrafficDoesNotBlockHeartbeat
  TestActualHeartbeatReconcilesLegacyCountersOnlyOnce
  TestInterruptedCreditRecoveryIsIdempotent
  TestTrafficExcludesConcurrentUnusedStreamCredits
  TestStreamCreditRefundUsesReservationDay
  TestConcurrentStreamAccountingWithTrafficReads
  TestDeleteSubscriptionAndUnusedPlan
  TestSubscriptionAtomicBudgetAndConcurrent
  TestConcurrentPacingSharesAccountBudget
  TestSubscriptionRateAndReset
  TestSubscriptionAgentAuthorizations
  TestSubscriptionFinishRefundsPartialChunkOnce
  TestChargeRefundKeepsPreviousBillingCycle
  TestSubscriptionCleanupRemovesRevokedTargets
  TestUsageRetentionPreservesCurrentAndUnsettledCycles
  TestUsageCleanupProtectsLegacyLeaseAndDeletedAccountSettlement
  TestSubscriptionRefundUsesChargedCycleAfterReset
  TestUsageCleanupPreservesUnparseableAccount
  TestSubscriptionRefundRejectsMissingOrStaleReservation
)
if [[ "$mode" == --list ]]; then
  printf '%s\n' "${tests[@]}"
  exit 0
fi
[[ -n "${EMBYRELAY_TEST_POSTGRES_URL:-}" ]] || {
  echo 'Set EMBYRELAY_TEST_POSTGRES_URL to a disposable PostgreSQL test database.' >&2
  exit 2
}
if [[ "$mode" == --full ]]; then
  exec go test -mod=readonly -race ./internal/control ./internal/database ./migrations
fi
pattern=$(IFS='|'; echo "^(${tests[*]})$")
# Fail if a selected test was renamed/deleted instead of passing with no tests.
listed=$(go test -mod=readonly -list "$pattern" ./internal/control)
for test_name in "${tests[@]}"; do
  [[ $'\n'"$listed"$'\n' == *$'\n'"$test_name"$'\n'* ]] || {
    echo "Selected PostgreSQL test is missing: $test_name" >&2
    exit 1
  }
done
echo "PostgreSQL focused regression: ${#tests[@]} tests"
exec go test -mod=readonly -race -run "$pattern" ./internal/control
