import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:go_router/go_router.dart';
import 'package:sovereign_wallet/app/providers.dart';
import 'package:sovereign_wallet/app/theme/app_theme.dart';
import 'package:sovereign_wallet/core/money/money.dart';
import 'package:sovereign_wallet/core/networking/api_error.dart';
import 'package:sovereign_wallet/core/networking/idempotency.dart';
import 'package:sovereign_wallet/core/security/token_store.dart';
import 'package:sovereign_wallet/features/auth/data/auth_repository.dart';
import 'package:sovereign_wallet/features/auth/domain/user.dart';
import 'package:sovereign_wallet/features/deposits/data/payments_repository.dart';
import 'package:sovereign_wallet/features/deposits/domain/payment.dart';
import 'package:sovereign_wallet/features/notifications/data/notifications_repository.dart';
import 'package:sovereign_wallet/features/statements/data/statements_repository.dart';
import 'package:sovereign_wallet/features/statements/presentation/statements_screen.dart';
import 'package:sovereign_wallet/features/transactions/data/transactions_repository.dart';
import 'package:sovereign_wallet/features/transactions/domain/models.dart';
import 'package:sovereign_wallet/features/wallet/data/wallet_repository.dart';
import 'package:sovereign_wallet/features/wallet/domain/models.dart';

Money kes(String s) => Money(Money.parseMinor(s, Currency.kes), Currency.kes);
Map<String, String> wire(String s) => {'amount': s, 'currency': 'KES'};

const testUser = AppUser(
  id: 'u1', phone: '+254712345678', fullName: 'Amina Otieno', role: 'user', status: 'active',
  kycTier: 2, phoneVerified: true, hasPin: true,
);

class FakeAuthRepository implements AuthRepository {
  FakeAuthRepository([this.user = testUser]);
  AppUser? user;
  @override
  Future<AppUser?> restore() async => user;
  @override
  Future<AppUser> me() async => user!;
  @override
  Future<void> logout() async => user = null;
  @override
  Future<AppUser> login(String phone, String password) async => user = testUser;
  @override
  Future<AppUser> register({required String phone, required String fullName, required String password, String? email}) async => testUser;
  @override
  Future<AppUser> verifyPhone(String code) async => testUser;
  @override
  Future<void> resendCode() async {}
  @override
  Future<void> setPin(String password, String pin) async {}
  @override
  Future<void> changePassword(String current, String next) async {}
  @override
  Future<void> forgotPassword(String phone) async {}
  @override
  Future<void> resetPassword(String phone, String code, String newPassword) async {}
}

class FakeWalletRepository implements WalletRepository {
  Balance balanceValue = Balance(available: kes('84250.00'), held: kes('3500.00'), total: kes('87750.00'), pendingInbound: kes('0'), asOf: DateTime.utc(2026));
  final quotes = <String>[];
  Money transferFee = kes('0');
  Money withdrawalFee = kes('20.00');

  @override
  Future<Wallet> primaryWallet() async => const Wallet(id: 'w1', accountNumber: '100000008294', currency: 'KES', status: 'active');
  @override
  Future<Balance> balance(String walletId) async => balanceValue;
  @override
  Future<Limits> limits() async => Limits.fromJson({
        'kyc_tier': 2,
        'verification': {'phone_verified': true},
        'limits': [
          for (final op in ['deposit', 'transfer', 'withdrawal'])
            {'operation': op, 'currency': 'KES', 'per_transaction_min': '10.00', 'per_transaction_max': '150000.00', 'daily_max': '300000.00', 'used_today': '0.00', 'remaining_today': '300000.00'},
        ],
      });
  @override
  Future<List<ProviderInfo>> providers() async => [
        const ProviderInfo(name: 'mpesa', environment: 'sandbox', collection: true, payout: true),
      ];
  @override
  Future<Quote> quote({required String operation, required String amount, String? provider}) async {
    quotes.add('$operation:$amount');
    final a = kes(amount);
    final fee = operation == 'withdrawal' ? withdrawalFee : transferFee;
    return Quote(amount: a, fee: fee, total: a + fee);
  }
}

Activity activity(String id, {String type = 'deposit', String direction = 'credit', String amount = '100.00', String status = 'succeeded', String? counterparty, DateTime? at}) =>
    Activity.fromJson({
      'id': id, 'type': type, 'direction': direction, 'amount': wire(amount), 'fee': wire('0.00'), 'status': status,
      'display_status': status, 'provider': type == 'deposit' || type == 'withdrawal' ? 'mpesa' : null,
      'counterparty': counterparty, 'description': '', 'created_at': (at ?? DateTime.now().toUtc()).toIso8601String(),
    });

class FakeTransactionsRepository implements TransactionsRepository {
  FakeTransactionsRepository(this.items);
  final List<Activity> items;
  final queries = <ActivityQuery>[];

  @override
  Future<ActivityPage> list(ActivityQuery query, {String? cursor, int limit = 30}) async {
    queries.add(query);
    var r = items;
    if (query.type == 'transfer') {
      r = r.where((a) => a.type.startsWith('transfer')).toList();
    } else if (query.type != null) {
      r = r.where((a) => a.type == query.type).toList();
    }
    if (query.search.isNotEmpty) r = r.where((a) => (a.counterparty ?? '').toLowerCase().contains(query.search.toLowerCase())).toList();
    return ActivityPage(r, null);
  }

  @override
  Future<Receipt> receipt(String id) async => Receipt(activity: items.firstWhere((a) => a.id == id), receiptNumber: 'ST-1', walletAccount: '100000008294');
}

PaymentStatus payment({String kind = 'deposit', String status = 'pending', String display = 'pending', String? reason, String env = 'sandbox', bool uncertain = false}) =>
    PaymentStatus.fromJson(kind, {
      'id': 'p1', 'provider': 'mpesa', 'provider_environment': env, 'amount': wire('5000.00'),
      if (kind == 'withdrawal') 'fee': wire('20.00'), if (kind == 'withdrawal') 'total': wire('5020.00'),
      'status': status, 'display_status': display, 'phone_masked': '+254 712 ••• 678', 'failure_reason': reason, 'outcome_uncertain': uncertain,
    });

/// Programmable payments fake: queue responses or errors per call.
class FakePaymentsRepository implements PaymentsRepository {
  final keys = <String>[];
  final pins = <String>[];
  final responses = <Object>[]; // PaymentStatus | TransferResult | ApiException
  Recipient recipient = const Recipient(displayName: 'Bob O.', phoneMasked: '+254 723 ••• 114', accountNumber: '100000001234');

  Object _next() {
    final r = responses.removeAt(0);
    if (r is ApiException) throw r;
    return r;
  }

  @override
  Future<PaymentStatus> createDeposit(PaymentAttempt attempt, {required String provider, required String phone, required String amount}) async {
    keys.add(attempt.key);
    return _next() as PaymentStatus;
  }

  @override
  Future<PaymentStatus> deposit(String id) async => _next() as PaymentStatus;

  @override
  Future<PaymentStatus> createWithdrawal(PaymentAttempt attempt, {required String provider, required String amount, required String pin}) async {
    keys.add(attempt.key);
    pins.add(pin);
    return _next() as PaymentStatus;
  }

  @override
  Future<PaymentStatus> withdrawal(String id) async => _next() as PaymentStatus;

  @override
  Future<Recipient> lookupRecipient({String? phone, String? accountNumber}) async => recipient;

  @override
  Future<TransferResult> createTransfer(PaymentAttempt attempt, {String? phone, String? accountNumber, required String amount, required String pin, String description = ''}) async {
    keys.add(attempt.key);
    pins.add(pin);
    return _next() as TransferResult;
  }
}

class FakeNotificationsRepository implements NotificationsRepository {
  @override
  Future<NotificationFeed> list() async => const NotificationFeed([], 0);
  @override
  Future<void> markAllRead() async {}
}

class FakeStatementsRepository implements StatementsRepository {
  final generated = <String>[];
  @override
  Future<List<StatementInfo>> list() async => [];
  @override
  Future<StatementInfo> generate({required DateTime from, required DateTime to, required String format}) async {
    generated.add(format);
    return StatementInfo(id: 's1', periodStart: '2026-09-11', periodEnd: '2026-10-10', format: format, sizeBytes: 10, createdAt: DateTime.utc(2026));
  }

  @override
  Future<List<int>> download(String id) async => [1, 2, 3];
}

class Fakes {
  final auth = FakeAuthRepository();
  final wallet = FakeWalletRepository();
  final payments = FakePaymentsRepository();
  final statements = FakeStatementsRepository();
  final saved = <String>[];
  FakeTransactionsRepository transactions = FakeTransactionsRepository([]);

  List<Override> get overrides => [
        tokenStoreProvider.overrideWithValue(MemoryTokenStore()),
        authRepositoryProvider.overrideWithValue(auth),
        walletRepositoryProvider.overrideWithValue(wallet),
        paymentsRepositoryProvider.overrideWithValue(payments),
        transactionsRepositoryProvider.overrideWithValue(transactions),
        notificationsRepositoryProvider.overrideWithValue(FakeNotificationsRepository()),
        statementsRepositoryProvider.overrideWithValue(statements),
        statementSaverProvider.overrideWithValue((name, bytes) async => saved.add(name)),
      ];
}

/// Pumps [screen] at '/' inside a router; any other route renders its path so
/// tests can assert navigation.
Future<void> pumpScreen(WidgetTester tester, Widget screen, Fakes fakes) async {
  tester.view.physicalSize = const Size(1080, 2400);
  tester.view.devicePixelRatio = 2.5;
  addTearDown(tester.view.reset);
  final router = GoRouter(routes: [
    GoRoute(path: '/', builder: (_, _) => screen),
    GoRoute(path: '/:a', builder: (_, s) => Scaffold(body: Text('route:${s.uri}'))),
    GoRoute(path: '/:a/:b', builder: (_, s) => Scaffold(body: Text('route:${s.uri}'))),
    GoRoute(path: '/:a/:b/:c', builder: (_, s) => Scaffold(body: Text('route:${s.uri}'))),
  ]);
  await tester.pumpWidget(ProviderScope(
    overrides: fakes.overrides,
    child: MaterialApp.router(theme: buildTheme(), routerConfig: router),
  ));
  await tester.pumpAndSettle();
}
