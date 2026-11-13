import '../../../core/money/money.dart';
import '../../transactions/domain/models.dart';

/// Server state of an external payment (deposit or withdrawal).
class PaymentStatus {
  const PaymentStatus({
    required this.id,
    required this.kind,
    required this.provider,
    required this.environment,
    required this.amount,
    required this.status,
    required this.displayStatus,
    required this.phoneMasked,
    this.fee,
    this.total,
    this.receipt,
    this.failureReason,
    this.outcomeUncertain = false,
  });

  final String id;
  final String kind; // deposit | withdrawal
  final String provider;
  final String environment;
  final Money amount;
  final Money? fee;
  final Money? total;
  final String status;
  final DisplayStatus displayStatus;
  final String phoneMasked;
  final String? receipt;
  final String? failureReason;
  final bool outcomeUncertain;

  bool get simulated => environment == 'simulated';

  factory PaymentStatus.fromJson(String kind, Map<String, dynamic> j) => PaymentStatus(
        id: j['id'] as String,
        kind: kind,
        provider: j['provider'] as String,
        environment: j['provider_environment'] as String,
        amount: Money.fromJson((j['amount'] as Map).cast()),
        fee: j['fee'] == null ? null : Money.fromJson((j['fee'] as Map).cast()),
        total: j['total'] == null ? null : Money.fromJson((j['total'] as Map).cast()),
        status: j['status'] as String,
        displayStatus: DisplayStatus.parse(j['display_status'] as String?),
        phoneMasked: (j['phone_masked'] as String?) ?? '',
        receipt: j['provider_receipt'] as String?,
        failureReason: j['failure_reason'] as String?,
        outcomeUncertain: (j['outcome_uncertain'] as bool?) ?? false,
      );
}
