import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:sovereign_wallet/app/router.dart';
import 'package:sovereign_wallet/app/theme/app_theme.dart';
import 'package:sovereign_wallet/core/networking/api_error.dart';
import 'package:sovereign_wallet/core/widgets/money_widgets.dart';
import 'package:sovereign_wallet/features/auth/domain/user.dart';
import 'package:sovereign_wallet/features/auth/presentation/session_controller.dart';
import 'package:sovereign_wallet/features/deposits/data/payments_repository.dart';
import 'package:sovereign_wallet/features/deposits/presentation/deposit_screen.dart';
import 'package:sovereign_wallet/features/deposits/presentation/payment_flow.dart';
import 'package:sovereign_wallet/features/statements/presentation/statements_screen.dart';
import 'package:sovereign_wallet/features/transactions/domain/models.dart';
import 'package:sovereign_wallet/features/transactions/presentation/activity_screen.dart';
import 'package:sovereign_wallet/features/transfers/presentation/transfer_screen.dart';
import 'package:sovereign_wallet/features/wallet/presentation/home_screen.dart';
import 'package:sovereign_wallet/features/withdrawals/presentation/withdraw_screen.dart';

import 'helpers.dart';

Widget _wrap(Widget w) => MaterialApp(theme: buildTheme(), home: Scaffold(body: SingleChildScrollView(child: w)));

void main() {
  group('Balance & status rendering', () {
    testWidgets('balance card shows server balance with tabular KES formatting and can hide it', (tester) async {
      var hidden = false;
      await tester.pumpWidget(StatefulBuilder(
        builder: (_, set) => _wrap(BalanceCard(
          label: 'Primary wallet', available: kes('84250.00'), held: kes('3500.00'), hidden: hidden,
          onToggleHidden: () => set(() => hidden = !hidden),
        )),
      ));
      expect(find.text('KES 84,250.00'), findsWidgets);
      expect(find.textContaining('3,500.00'), findsOneWidget);
      await tester.tap(find.byTooltip('Hide balance'));
      await tester.pump();
      expect(find.textContaining('84,250.00'), findsNothing);
      expect(find.text('KES ••••••'), findsOneWidget);
    });

    testWidgets('status chips distinguish pending, failed and unknown outcomes', (tester) async {
      await tester.pumpWidget(_wrap(const Column(children: [
        StatusChip(DisplayStatus.succeeded), StatusChip(DisplayStatus.pending), StatusChip(DisplayStatus.failed), StatusChip(DisplayStatus.unknown),
      ])));
      for (final label in ['Successful', 'Pending', 'Failed', 'Confirming']) {
        expect(find.text(label), findsOneWidget);
      }
    });

    testWidgets('home screen renders balance from the repository', (tester) async {
      final f = Fakes();
      f.transactions = FakeTransactionsRepository([activity('a1', counterparty: null)]);
      await pumpScreen(tester, const HomeScreen(), f);
      expect(find.byKey(const Key('balance.available')), findsOneWidget);
      expect(find.textContaining('84,250.00'), findsWidgets);
      expect(find.text('M-Pesa Top Up'), findsOneWidget);
    });
  });

  group('Deposit flow', () {
    testWidgets('validates amount before calling the API', (tester) async {
      final f = Fakes();
      await pumpScreen(tester, const DepositScreen(), f);
      await tester.ensureVisible(find.byKey(const Key('deposit.submit')));
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const Key('deposit.submit')));
      await tester.pump();
      expect(find.text('Enter an amount'), findsOneWidget);
      await tester.enterText(find.byKey(const Key('money.input')), '5');
      await tester.tap(find.byKey(const Key('deposit.submit')));
      await tester.pump();
      expect(find.textContaining('Minimum'), findsOneWidget);
      expect(f.payments.keys, isEmpty);
    });

    testWidgets('timeout shows unknown outcome; check status retries with the same key', (tester) async {
      final f = Fakes();
      f.payments.responses.addAll([
        ApiException(code: 'timeout', message: 'slow', outcomeUnknown: true),
        payment(),
      ]);
      await pumpScreen(tester, const DepositScreen(), f);
      expect(find.text('Enter your phone number'), findsNothing);
      await tester.enterText(find.byKey(const Key('money.input')), '5000');
      await tester.ensureVisible(find.byKey(const Key('deposit.submit')));
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const Key('deposit.submit')));
      await tester.pumpAndSettle();
      expect(find.byKey(const Key('outcome.unknown')), findsOneWidget);
      expect(find.text('We could not confirm your request'), findsOneWidget);
      await tester.ensureVisible(find.text('Check status'));
      await tester.pumpAndSettle();
      await tester.tap(find.text('Check status'));
      await tester.pumpAndSettle();
      expect(f.payments.keys.length, 2);
      expect(f.payments.keys[0], f.payments.keys[1], reason: 'retry must reuse the idempotency key');
      expect(find.text('route:/payments/deposit/p1'), findsOneWidget);
    });

    testWidgets('definitive rejection shows the server message', (tester) async {
      final f = Fakes();
      f.payments.responses.add(ApiException(code: 'limit_exceeded', message: 'daily limit exceeded', status: 422));
      await pumpScreen(tester, const DepositScreen(), f);
      expect(find.text('Enter your phone number'), findsNothing);
      await tester.enterText(find.byKey(const Key('money.input')), '5000');
      await tester.ensureVisible(find.byKey(const Key('deposit.submit')));
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const Key('deposit.submit')));
      await tester.pumpAndSettle();
      expect(find.text('daily limit exceeded'), findsOneWidget);
      expect(find.byKey(const Key('outcome.unknown')), findsNothing);
    });
  });

  group('Transfer flow', () {
    Future<void> fill(WidgetTester tester) async {
      await tester.enterText(find.byKey(const Key('transfer.to')), '0723000114');
      await tester.tap(find.byTooltip('Find recipient'));
      await tester.pumpAndSettle();
      expect(find.byKey(const Key('transfer.recipient')), findsOneWidget);
      await tester.enterText(find.byKey(const Key('money.input')), '3200');
      await tester.pump(const Duration(milliseconds: 400));
      await tester.pumpAndSettle();
    }

    Future<void> enterPin(WidgetTester tester, String pin) async {
      for (final d in pin.split('')) {
        await tester.tap(find.byKey(Key('pin.$d')));
        await tester.pump();
      }
      await tester.tap(find.byKey(const Key('pin.submit')));
      await tester.pumpAndSettle();
    }

    testWidgets('shows server fee and total, confirms with PIN, and completes', (tester) async {
      final f = Fakes();
      f.wallet.transferFee = kes('10.00');
      f.payments.responses.add(TransferResult(id: 't1', status: 'succeeded', amount: kes('3200'), fee: kes('10'), total: kes('3210'), counterparty: 'Bob O.'));
      await pumpScreen(tester, const TransferScreen(), f);
      await fill(tester);
      expect(find.byKey(const Key('fee.total')), findsOneWidget);
      expect(find.textContaining('3,210.00'), findsWidgets);
      await tester.ensureVisible(find.byKey(const Key('transfer.submit')));
      await tester.tap(find.byKey(const Key('transfer.submit')));
      await tester.pumpAndSettle();
      expect(find.byKey(const Key('confirm.total')), findsOneWidget);
      await enterPin(tester, '2580');
      expect(f.payments.pins, ['2580']);
      expect(find.text('route:/transfer/done'), findsOneWidget);
    });

    testWidgets('wrong PIN error is shown and retry keeps the same key', (tester) async {
      final f = Fakes();
      f.payments.responses.addAll([
        ApiException(code: 'invalid_pin', message: 'incorrect PIN; 4 attempts remaining', status: 401),
        TransferResult(id: 't1', status: 'succeeded', amount: kes('3200'), fee: kes('0'), total: kes('3200'), counterparty: 'Bob O.'),
      ]);
      await pumpScreen(tester, const TransferScreen(), f);
      await fill(tester);
      await tester.ensureVisible(find.byKey(const Key('transfer.submit')));
      await tester.tap(find.byKey(const Key('transfer.submit')));
      await tester.pumpAndSettle();
      await enterPin(tester, '1397');
      expect(find.text('incorrect PIN; 4 attempts remaining'), findsOneWidget);
      await tester.ensureVisible(find.byKey(const Key('transfer.submit')));
      await tester.tap(find.byKey(const Key('transfer.submit')));
      await tester.pumpAndSettle();
      await enterPin(tester, '2580');
      expect(f.payments.keys.toSet().length, 1, reason: 'same logical transfer, same key');
    });
  });

  group('Withdrawal flow', () {
    testWidgets('shows fee and total before PIN confirmation', (tester) async {
      final f = Fakes();
      f.payments.responses.add(payment(kind: 'withdrawal'));
      await pumpScreen(tester, const WithdrawScreen(), f);
      await tester.enterText(find.byKey(const Key('money.input')), '5000');
      await tester.pump(const Duration(milliseconds: 400));
      await tester.pumpAndSettle();
      expect(find.text('Withdrawal fee'), findsOneWidget);
      expect(find.textContaining('5,020.00'), findsWidgets);
      await tester.ensureVisible(find.byKey(const Key('withdraw.submit')));
      await tester.tap(find.byKey(const Key('withdraw.submit')));
      await tester.pumpAndSettle();
      for (final d in '2580'.split('')) {
        await tester.tap(find.byKey(Key('pin.$d')));
      }
      await tester.pump();
      await tester.tap(find.byKey(const Key('pin.submit')));
      await tester.pumpAndSettle();
      expect(find.text('route:/payments/withdrawal/p1'), findsOneWidget);
    });

    testWidgets('fractional amounts are rejected for mobile money', (tester) async {
      final f = Fakes();
      await pumpScreen(tester, const WithdrawScreen(), f);
      await tester.enterText(find.byKey(const Key('money.input')), '100.50');
      await tester.pump();
      // The input formatter strips '.', so the decimal can't be typed at all.
      expect(find.text('10050'), findsOneWidget);
    });
  });

  group('Payment status', () {
    Future<void> show(WidgetTester tester, PaymentStatusView v) => tester.pumpWidget(MaterialApp(theme: buildTheme(), home: Scaffold(body: v)));

    testWidgets('pending deposit instructs to approve the prompt and never asks for the PIN', (tester) async {
      await show(tester, PaymentStatusView(status: payment(), onDone: () {}));
      expect(find.text('Check your phone'), findsOneWidget);
      expect(find.textContaining('we will never ask for it'), findsOneWidget);
    });

    testWidgets('failed withdrawal says funds were returned', (tester) async {
      await show(tester, PaymentStatusView(status: payment(kind: 'withdrawal', status: 'failed', display: 'failed', reason: 'Payout failed'), onDone: () {}));
      expect(find.text('Withdrawal failed'), findsOneWidget);
      expect(find.textContaining('returned to your wallet'), findsOneWidget);
    });

    testWidgets('unknown withdrawal outcome says funds are held, not lost', (tester) async {
      await show(tester, PaymentStatusView(status: payment(kind: 'withdrawal', status: 'processing', display: 'unknown', uncertain: true), onDone: () {}));
      expect(find.text('Confirming with M-Pesa'), findsOneWidget);
      expect(find.textContaining('stay held'), findsOneWidget);
    });

    testWidgets('simulated payments are labelled', (tester) async {
      await show(tester, PaymentStatusView(status: payment(env: 'simulated', status: 'succeeded', display: 'succeeded'), onDone: () {}));
      expect(find.textContaining('Simulated payments'), findsOneWidget);
      expect(find.text('Deposit received'), findsOneWidget);
    });
  });

  group('Transaction history', () {
    testWidgets('filters by type and searches', (tester) async {
      final f = Fakes();
      f.transactions = FakeTransactionsRepository([
        activity('d1', type: 'deposit'),
        activity('t1', type: 'transfer_out', direction: 'debit', counterparty: 'Faith W.'),
        activity('w1', type: 'withdrawal', direction: 'debit', status: 'pending'),
      ]);
      await pumpScreen(tester, const ActivityScreen(), f);
      expect(find.byType(TransactionRow), findsNWidgets(3));
      await tester.tap(find.byKey(const Key('filter.type.transfer')));
      await tester.pumpAndSettle();
      expect(f.transactions.queries.last.type, 'transfer');
      expect(find.byType(TransactionRow), findsOneWidget);
      expect(find.text('Faith W.'), findsOneWidget);
      await tester.tap(find.byKey(const Key('filter.type.all')));
      await tester.pumpAndSettle();
      await tester.enterText(find.byKey(const Key('activity.search')), 'faith');
      await tester.pump(const Duration(milliseconds: 400));
      await tester.pumpAndSettle();
      expect(f.transactions.queries.last.search, 'faith');
      expect(find.byType(TransactionRow), findsOneWidget);
    });
  });

  group('Statements', () {
    testWidgets('generates and saves a statement', (tester) async {
      final f = Fakes();
      await pumpScreen(tester, const StatementsScreen(), f);
      await tester.tap(find.byKey(const Key('statements.generate')));
      await tester.pumpAndSettle();
      expect(f.statements.generated, ['pdf']);
      expect(f.saved.single, endsWith('.pdf'));
    });
  });

  group('Route guards', () {
    const unverified = AppUser(id: 'u', phone: '+254700000000', fullName: 'X Y', role: 'user', status: 'pending_verification', kycTier: 1, phoneVerified: false, hasPin: false);
    const noPin = AppUser(id: 'u', phone: '+254700000000', fullName: 'X Y', role: 'user', status: 'active', kycTier: 1, phoneVerified: true, hasPin: false);

    test('unauthenticated users are sent to login', () {
      expect(guard(const SignedOut(), '/home', admin: false), '/login');
      expect(guard(const SignedOut(), '/deposit', admin: false), '/login');
      expect(guard(const SignedOut(), '/register', admin: false), isNull);
    });

    test('session expiry while on a money screen redirects to login', () {
      expect(guard(const SignedOut('Your session expired.'), '/payments/deposit/1', admin: false), '/login');
    });

    test('verification and PIN setup are required before money screens', () {
      expect(guard(const SignedIn(unverified), '/deposit', admin: false), '/verify-phone');
      expect(guard(const SignedIn(noPin), '/transfer', admin: false), '/set-pin');
      expect(guard(const SignedIn(testUser), '/login', admin: false), '/home');
      expect(guard(const SignedIn(testUser), '/transfer', admin: false), isNull);
    });

    test('admin console rejects non-staff users', () {
      expect(guard(const SignedIn(testUser), '/admin', admin: true), '/forbidden');
      const staff = AppUser(id: 'a', phone: '+254700000001', fullName: 'Ops A', role: 'support', status: 'active', kycTier: 1, phoneVerified: true, hasPin: false);
      expect(guard(const SignedIn(staff), '/admin/payments', admin: true), isNull);
      expect(guard(const SignedIn(staff), '/login', admin: true), '/admin');
    });
  });
}
