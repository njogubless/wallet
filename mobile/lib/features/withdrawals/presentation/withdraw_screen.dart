import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../app/providers.dart';
import '../../../app/theme/app_theme.dart';
import '../../../app/theme/tokens.dart';
import '../../../core/formatting/formatters.dart';
import '../../../core/widgets/common.dart';
import '../../../core/widgets/confirmation_sheet.dart';
import '../../../core/widgets/money_widgets.dart';
import '../../auth/presentation/session_controller.dart';
import '../../deposits/presentation/payment_flow.dart';
import '../../wallet/domain/models.dart';
import '../../wallet/presentation/wallet_providers.dart';

class WithdrawScreen extends ConsumerStatefulWidget {
  const WithdrawScreen({super.key});
  @override
  ConsumerState<WithdrawScreen> createState() => _WithdrawScreenState();
}

class _WithdrawScreenState extends ConsumerState<WithdrawScreen> {
  final _form = GlobalKey<FormState>();
  final _amount = TextEditingController();
  final _submitter = PaymentSubmitter();
  final _debounce = Debouncer();
  String? _provider;
  Quote? _quote;
  String? _error;
  bool _unknown = false;
  bool _busy = false;
  String? _lastPin;

  @override
  void dispose() {
    _debounce.dispose();
    super.dispose();
  }

  void _refreshQuote() {
    final v = _amount.text.trim();
    if (_provider == null || MoneyInput.validate(v, wholeUnitsOnly: true) != null) {
      setState(() => _quote = null);
      return;
    }
    _debounce(const Duration(milliseconds: 300), () async {
      try {
        final q = await ref.read(walletRepositoryProvider).quote(operation: 'withdrawal', amount: v, provider: _provider);
        if (mounted && _amount.text.trim() == v) setState(() => _quote = q);
      } catch (_) {
        if (mounted) setState(() => _quote = null);
      }
    });
  }

  Future<void> _authorize() async {
    if (_busy || _quote == null || !_form.currentState!.validate()) return;
    final phone = ref.read(currentUserProvider)?.phone ?? '';
    final pin = await showPinConfirmation(context,
        title: 'Authorize withdrawal',
        details: [('To', '${providerLabel(_provider)} $phone'), ('Amount', _quote!.amount.format()), ('Fee', _quote!.fee.format())],
        total: _quote!.total);
    if (pin == null || !mounted) return;
    _lastPin = pin;
    await _send(pin);
  }

  Future<void> _send(String pin) async {
    setState(() {
      _busy = true;
      _error = null;
    });
    final amount = _amount.text.trim();
    final result = await _submitter.submit(
      {'provider': _provider, 'amount': amount},
      (attempt) => ref.read(paymentsRepositoryProvider).createWithdrawal(attempt, provider: _provider!, amount: amount, pin: pin),
    );
    if (!mounted) return;
    setState(() => _busy = false);
    switch (result) {
      case Submitted(:final value):
        invalidateMoney(ref);
        context.pushReplacement('/payments/withdrawal/${value.id}');
      case Rejected(:final error):
        setState(() {
          _unknown = false;
          _error = error.message;
        });
      case OutcomeUnknown():
        setState(() => _unknown = true);
      case null:
        break;
    }
  }

  @override
  Widget build(BuildContext context) {
    final providers = ref.watch(paymentProvidersProvider);
    final balance = ref.watch(balanceProvider).valueOrNull;
    final limits = ref.watch(limitsProvider).valueOrNull?.of('withdrawal');
    final user = ref.watch(currentUserProvider);
    final available = providers.valueOrNull?.where((p) => p.payout).toList() ?? [];
    if (_provider == null && available.isNotEmpty) _provider = available.first.name;
    return Scaffold(
      appBar: AppBar(title: const Text('Withdraw')),
      body: providers.when(
        loading: () => const LoadingView(),
        error: (e, _) => ErrorView(error: e, onRetry: () => ref.invalidate(paymentProvidersProvider)),
        data: (_) => available.isEmpty
            ? const EmptyView(message: 'Withdrawals are not available right now.', icon: Icons.portable_wifi_off)
            : Form(
                key: _form,
                child: ListView(padding: const EdgeInsets.all(AppSpace.md), children: [
                  if (available.any((p) => p.simulated)) ...[const SimulatedBanner(), const SizedBox(height: AppSpace.md)],
                  const SectionLabel('Send to'),
                  ProviderPicker(providers: available.map((p) => p.name).toList(), selected: _provider, onChanged: (p) {
                    setState(() => _provider = p);
                    _refreshQuote();
                  }),
                  const SizedBox(height: AppSpace.sm),
                  Text('Withdrawals go only to your registered number ${user?.phone ?? ''}.', style: AppText.bodySm.copyWith(color: AppColors.secondary)),
                  const SizedBox(height: AppSpace.lg),
                  SectionLabel('Amount', trailing: balance == null ? null : Text('Available: ${balance.available.format()}', style: AppText.labelMd.copyWith(color: AppColors.success))),
                  MoneyInput(controller: _amount, wholeUnitsOnly: true, min: limits?.perTxnMin, max: limits?.perTxnMax, onChanged: (_) => _refreshQuote()),
                  const SizedBox(height: AppSpace.md),
                  if (_quote != null)
                    FeeSummary(rows: [('Amount', _quote!.amount), ('Withdrawal fee', _quote!.fee)], totalLabel: 'Total deducted', total: _quote!.total),
                  const SizedBox(height: AppSpace.md),
                  Text('Funds are held as soon as you confirm and are returned automatically if the payout definitively fails.',
                      style: AppText.bodySm.copyWith(color: AppColors.muted)),
                  const SizedBox(height: AppSpace.lg),
                  if (_error != null) Padding(padding: const EdgeInsets.only(bottom: AppSpace.md), child: Text(_error!, key: const Key('withdraw.error'), style: AppText.bodyMd.copyWith(color: AppColors.danger))),
                  if (_unknown)
                    UnknownOutcomeCard(busy: _busy, onCheck: () => _lastPin == null ? _authorize() : _send(_lastPin!))
                  else
                    FilledButton.icon(
                      key: const Key('withdraw.submit'),
                      onPressed: _busy || _quote == null ? null : _authorize,
                      icon: const Icon(Icons.lock_outline),
                      label: Text(_quote == null ? 'Withdraw' : 'Withdraw ${_quote!.amount.format()}'),
                    ),
                ]),
              ),
      ),
    );
  }
}
