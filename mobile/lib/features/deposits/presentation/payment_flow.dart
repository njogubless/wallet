import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../app/providers.dart';
import '../../../app/theme/app_theme.dart';
import '../../../app/theme/tokens.dart';
import '../../../core/formatting/formatters.dart';
import '../../../core/networking/api_error.dart';
import '../../../core/networking/idempotency.dart';
import '../../../core/widgets/common.dart';
import '../../../core/widgets/money_widgets.dart';
import '../../transactions/domain/models.dart';
import '../../wallet/presentation/wallet_providers.dart';
import '../domain/payment.dart';

/// Outcome of submitting a money-moving request.
sealed class SubmitResult<T> {
  const SubmitResult();
}

class Submitted<T> extends SubmitResult<T> {
  const Submitted(this.value);
  final T value;
}

class Rejected<T> extends SubmitResult<T> {
  const Rejected(this.error);
  final ApiException error;
}

/// The request may or may not have executed (timeout / 5xx). Retrying with
/// the same attempt is safe: the server returns the original result.
class OutcomeUnknown<T> extends SubmitResult<T> {
  const OutcomeUnknown(this.error);
  final ApiException error;
}

/// Holds one logical payment's idempotency key across retries and guards
/// against concurrent submits. UI disabling is a convenience only; the key is
/// what makes retries safe.
class PaymentSubmitter {
  PaymentAttempt? _attempt;
  bool _inFlight = false;

  bool get inFlight => _inFlight;
  String? get currentKey => _attempt?.key;

  Future<SubmitResult<T>?> submit<T>(Map<String, Object?> params, Future<T> Function(PaymentAttempt) call) async {
    if (_inFlight) return null;
    _attempt = _attempt?.reuseOrRenew(params) ?? PaymentAttempt.forRequest(params);
    _inFlight = true;
    try {
      return Submitted(await call(_attempt!));
    } on ApiException catch (e) {
      return e.outcomeUnknown ? OutcomeUnknown(e) : Rejected(e);
    } finally {
      _inFlight = false;
    }
  }
}

class UnknownOutcomeCard extends StatelessWidget {
  const UnknownOutcomeCard({super.key, required this.onCheck, required this.busy});
  final VoidCallback onCheck;
  final bool busy;

  @override
  Widget build(BuildContext context) => AppCard(
        key: const Key('outcome.unknown'),
        color: AppColors.unknownBg,
        child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
          Row(children: [
            const Icon(Icons.help_outline, color: AppColors.unknown),
            const SizedBox(width: AppSpace.sm),
            Expanded(child: Text('We could not confirm your request', style: AppText.titleMd)),
          ]),
          const SizedBox(height: AppSpace.sm),
          Text('The connection dropped before we got an answer. Do not start a new payment - '
              'check the status instead. Checking is safe and will never charge you twice.',
              style: AppText.bodyMd.copyWith(color: AppColors.secondary)),
          const SizedBox(height: AppSpace.md),
          FilledButton(onPressed: busy ? null : onCheck, child: Text(busy ? 'Checking…' : 'Check status')),
        ]),
      );
}

/// Polls a deposit or withdrawal until a terminal state, showing
/// server-authoritative status only.
final paymentStatusProvider = StreamProvider.autoDispose.family<PaymentStatus, (String, String)>((ref, args) async* {
  final (kind, id) = args;
  final repo = ref.watch(paymentsRepositoryProvider);
  final started = DateTime.now();
  while (true) {
    PaymentStatus? s;
    try {
      s = kind == 'deposit' ? await repo.deposit(id) : await repo.withdrawal(id);
      yield s;
    } on ApiException catch (e) {
      if (e.isUnauthenticated || e.code == 'not_found') rethrow;
      // transient: keep polling
    }
    if (s != null && s.displayStatus.isTerminal) return;
    final age = DateTime.now().difference(started);
    await Future<void>.delayed(age < const Duration(minutes: 2) ? const Duration(seconds: 2) : const Duration(seconds: 10));
  }
});

class PaymentStatusScreen extends ConsumerStatefulWidget {
  const PaymentStatusScreen({super.key, required this.kind, required this.id});
  final String kind;
  final String id;
  @override
  ConsumerState<PaymentStatusScreen> createState() => _PaymentStatusScreenState();
}

class _PaymentStatusScreenState extends ConsumerState<PaymentStatusScreen> {
  bool _refreshedMoney = false;

  @override
  Widget build(BuildContext context) {
    final st = ref.watch(paymentStatusProvider((widget.kind, widget.id)));
    ref.listen(paymentStatusProvider((widget.kind, widget.id)), (_, next) {
      final v = next.valueOrNull;
      if (v != null && v.displayStatus.isTerminal && !_refreshedMoney) {
        _refreshedMoney = true;
        invalidateMoney(ref);
      }
    });
    return Scaffold(
      appBar: AppBar(title: Text(widget.kind == 'deposit' ? 'Deposit status' : 'Withdrawal status')),
      body: st.when(
        loading: () => const LoadingView(label: 'Checking status…'),
        error: (e, _) => ErrorView(error: e, onRetry: () => ref.invalidate(paymentStatusProvider((widget.kind, widget.id)))),
        data: (p) => PaymentStatusView(status: p, onDone: () => context.go('/home'), onReceipt: () => context.push('/transactions/${p.id}')),
      ),
    );
  }
}

class PaymentStatusView extends StatelessWidget {
  const PaymentStatusView({super.key, required this.status, required this.onDone, this.onReceipt});
  final PaymentStatus status;
  final VoidCallback onDone;
  final VoidCallback? onReceipt;

  @override
  Widget build(BuildContext context) {
    final p = status;
    final deposit = p.kind == 'deposit';
    final channel = providerLabel(p.provider);
    final (icon, color, title, body) = switch (p.displayStatus) {
      DisplayStatus.succeeded => (Icons.check_circle, AppColors.success, deposit ? 'Deposit received' : 'Withdrawal sent',
          deposit ? 'The money is now in your wallet.' : 'The money was sent to your $channel account ${p.phoneMasked}.'),
      DisplayStatus.failed => (Icons.cancel, AppColors.danger, deposit ? 'Deposit not completed' : 'Withdrawal failed',
          '${p.failureReason ?? 'The payment was not completed.'} ${deposit ? 'No money was added.' : 'Your funds were returned to your wallet.'}'),
      DisplayStatus.reversed => (Icons.undo, AppColors.muted, 'Reversed', 'This transaction was reversed.'),
      DisplayStatus.unknown => (Icons.help_outline, AppColors.unknown, 'Confirming with $channel',
          deposit
              ? 'We have not received confirmation yet. If money left your $channel account, it will be credited once confirmed.'
              : 'The provider has not confirmed the payout yet. Your funds stay held - they will not be lost or sent twice.'),
      DisplayStatus.pending => (Icons.hourglass_top_rounded, AppColors.pendingAccent, deposit ? 'Check your phone' : 'Sending your money',
          deposit
              ? 'Approve the $channel prompt sent to ${p.phoneMasked}. Enter your PIN only on the $channel prompt - we will never ask for it.'
              : 'We are sending ${p.amount.format()} to ${p.phoneMasked}. This usually takes under a minute.'),
    };
    return ListView(padding: const EdgeInsets.all(AppSpace.lg), children: [
      if (p.simulated) ...[const SimulatedBanner(), const SizedBox(height: AppSpace.lg)],
      Center(
        child: p.displayStatus == DisplayStatus.pending
            ? const SizedBox(width: 72, height: 72, child: CircularProgressIndicator(strokeWidth: 4))
            : Icon(icon, size: 72, color: color),
      ),
      const SizedBox(height: AppSpace.lg),
      Text(title, key: const Key('status.title'), textAlign: TextAlign.center, style: AppText.headlineMd),
      const SizedBox(height: AppSpace.sm),
      Text(body, textAlign: TextAlign.center, style: AppText.bodyMd.copyWith(color: AppColors.secondary)),
      const SizedBox(height: AppSpace.lg),
      AppCard(
        child: Column(children: [
          _row('Amount', MoneyText(p.amount, style: AppText.titleMd)),
          if (p.fee != null && !p.fee!.isZero) _row('Fee', MoneyText(p.fee!, style: AppText.bodyMd)),
          if (p.total != null) _row('Total', MoneyText(p.total!, style: AppText.titleMd)),
          _row('Status', StatusChip(p.displayStatus)),
          if (p.receipt != null) _row('Reference', Text(p.receipt!, style: AppText.labelLg)),
        ]),
      ),
      const SizedBox(height: AppSpace.lg),
      if (p.displayStatus.isTerminal || p.displayStatus == DisplayStatus.unknown) ...[
        FilledButton(onPressed: onDone, child: const Text('Done')),
        if (onReceipt != null) TextButton(onPressed: onReceipt, child: const Text('View receipt')),
      ] else
        OutlinedButton(onPressed: onDone, child: const Text('Continue in background')),
    ]);
  }

  Widget _row(String label, Widget value) => Padding(
        padding: const EdgeInsets.symmetric(vertical: 6),
        child: Row(children: [Expanded(child: Text(label, style: AppText.bodyMd.copyWith(color: AppColors.muted))), value]),
      );
}

class ProviderPicker extends StatelessWidget {
  const ProviderPicker({super.key, required this.providers, required this.selected, required this.onChanged});
  final List<String> providers;
  final String? selected;
  final ValueChanged<String> onChanged;

  @override
  Widget build(BuildContext context) => Row(children: [
        for (final p in providers)
          Expanded(
            child: Padding(
              padding: const EdgeInsets.symmetric(horizontal: 4),
              child: AppCard(
                color: selected == p ? AppColors.surfaceHigh : null,
                onTap: () => onChanged(p),
                child: Row(children: [
                  Icon(Icons.phone_android, color: p == 'mpesa' ? AppColors.success : AppColors.danger),
                  const SizedBox(width: AppSpace.sm),
                  Expanded(child: Text(providerLabel(p), style: AppText.labelLg)),
                  if (selected == p) const Icon(Icons.check_circle, color: AppColors.primary, size: 18),
                ]),
              ),
            ),
          ),
      ]);
}

/// Waits briefly then refreshes a quote, so fees shown always come from the server.
class Debouncer {
  Timer? _t;
  void call(Duration d, VoidCallback fn) {
    _t?.cancel();
    _t = Timer(d, fn);
  }

  void dispose() => _t?.cancel();
}
