import '../../../core/money/money.dart';
import '../../../core/networking/api_client.dart';
import '../../../core/networking/idempotency.dart';
import '../domain/payment.dart';

class Recipient {
  const Recipient({required this.displayName, required this.phoneMasked, required this.accountNumber});
  final String displayName;
  final String phoneMasked;
  final String accountNumber;

  factory Recipient.fromJson(Map<String, dynamic> j) => Recipient(
        displayName: j['display_name'] as String,
        phoneMasked: j['phone_masked'] as String,
        accountNumber: j['account_number'] as String,
      );
}

class TransferResult {
  const TransferResult({required this.id, required this.status, required this.amount, required this.fee, required this.total, required this.counterparty});
  final String id;
  final String status;
  final Money amount;
  final Money fee;
  final Money total;
  final String counterparty;

  factory TransferResult.fromJson(Map<String, dynamic> j) => TransferResult(
        id: j['id'] as String,
        status: j['status'] as String,
        amount: Money.fromJson((j['amount'] as Map).cast()),
        fee: Money.fromJson((j['fee'] as Map).cast()),
        total: Money.fromJson((j['total'] as Map).cast()),
        counterparty: j['counterparty_name'] as String,
      );
}

/// Money-moving calls. Every create call carries the attempt's idempotency
/// key so retries are safe.
abstract class PaymentsRepository {
  Future<PaymentStatus> createDeposit(PaymentAttempt attempt, {required String provider, required String phone, required String amount});
  Future<PaymentStatus> deposit(String id);
  Future<PaymentStatus> createWithdrawal(PaymentAttempt attempt, {required String provider, required String amount, required String pin});
  Future<PaymentStatus> withdrawal(String id);
  Future<Recipient> lookupRecipient({String? phone, String? accountNumber});
  Future<TransferResult> createTransfer(PaymentAttempt attempt,
      {String? phone, String? accountNumber, required String amount, required String pin, String description});
}

class ApiPaymentsRepository implements PaymentsRepository {
  ApiPaymentsRepository(this.api);
  final ApiClient api;

  @override
  Future<PaymentStatus> createDeposit(PaymentAttempt attempt, {required String provider, required String phone, required String amount}) async =>
      PaymentStatus.fromJson('deposit', await api.post('/api/v1/deposits',
          body: {'provider': provider, 'phone': phone, 'amount': amount}, idempotencyKey: attempt.key));

  @override
  Future<PaymentStatus> deposit(String id) async => PaymentStatus.fromJson('deposit', await api.get('/api/v1/deposits/$id'));

  @override
  Future<PaymentStatus> createWithdrawal(PaymentAttempt attempt, {required String provider, required String amount, required String pin}) async =>
      PaymentStatus.fromJson('withdrawal', await api.post('/api/v1/withdrawals',
          body: {'provider': provider, 'amount': amount, 'pin': pin}, idempotencyKey: attempt.key));

  @override
  Future<PaymentStatus> withdrawal(String id) async => PaymentStatus.fromJson('withdrawal', await api.get('/api/v1/withdrawals/$id'));

  @override
  Future<Recipient> lookupRecipient({String? phone, String? accountNumber}) async => Recipient.fromJson(await api.get(
      '/api/v1/recipients/lookup',
      query: {'phone': ?phone, 'account_number': ?accountNumber}));

  @override
  Future<TransferResult> createTransfer(PaymentAttempt attempt,
          {String? phone, String? accountNumber, required String amount, required String pin, String description = ''}) async =>
      TransferResult.fromJson(await api.post('/api/v1/transfers',
          body: {
            'recipient_phone': ?phone,
            'recipient_account': ?accountNumber,
            'amount': amount,
            'pin': pin,
            'description': description,
          },
          idempotencyKey: attempt.key));
}
