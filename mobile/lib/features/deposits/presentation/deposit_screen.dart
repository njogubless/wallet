import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../app/providers.dart';
import '../../../app/theme/app_theme.dart';
import '../../../app/theme/tokens.dart';
import '../../../core/formatting/formatters.dart';
import '../../../core/money/money.dart';
import '../../../core/widgets/common.dart';
import '../../../core/widgets/money_widgets.dart';
import '../../auth/presentation/auth_screens.dart' show validateKenyanPhone;
import '../../auth/presentation/session_controller.dart';
import '../../wallet/presentation/wallet_providers.dart';
import 'payment_flow.dart';

class DepositScreen extends ConsumerStatefulWidget {
  const DepositScreen({super.key});
  @override
  ConsumerState<DepositScreen> createState() => _DepositScreenState();
}

class _DepositScreenState extends ConsumerState<DepositScreen> {
  final _form = GlobalKey<FormState>();
  final _amount = TextEditingController();
  final _phone = TextEditingController();
  final _submitter = PaymentSubmitter();
  String? _provider;
  String? _error;
  bool _unknown = false;
  bool _busy = false;

  bool _phonePrefilled = false;

  Future<void> _submit() async {
    if (_busy || !_form.currentState!.validate() || _provider == null) return;
    final amount = _amount.text.trim();
    setState(() {
      _busy = true;
      _error = null;
    });
    final result = await _submitter.submit(
      {'provider': _provider, 'phone': _phone.text.trim(), 'amount': amount},
      (attempt) => ref.read(paymentsRepositoryProvider).createDeposit(attempt, provider: _provider!, phone: _phone.text.trim(), amount: amount),
    );
    if (!mounted) return;
    setState(() => _busy = false);
    switch (result) {
      case Submitted(:final value):
        invalidateMoney(ref);
        context.pushReplacement('/payments/deposit/${value.id}');
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
    final limits = ref.watch(limitsProvider).valueOrNull?.of('deposit');
    final user = ref.watch(currentUserProvider);
    if (!_phonePrefilled && user != null) {
      // Default to the account holder's number (+2547XXXXXXXX -> 07XXXXXXXX).
      _phonePrefilled = true;
      if (_phone.text.isEmpty) _phone.text = '0${user.phone.substring(4)}';
    }
    final available = providers.valueOrNull?.where((p) => p.collection).toList() ?? [];
    _provider ??= available.isNotEmpty ? available.first.name : null;
    final simulated = available.any((p) => p.simulated);
    return Scaffold(
      appBar: AppBar(title: const Text('Deposit funds')),
      body: providers.when(
        loading: () => const LoadingView(),
        error: (e, _) => ErrorView(error: e, onRetry: () => ref.invalidate(paymentProvidersProvider)),
        data: (_) => available.isEmpty
            ? const EmptyView(message: 'Deposits are not available right now.', icon: Icons.portable_wifi_off)
            : Form(
                key: _form,
                child: ListView(padding: const EdgeInsets.all(AppSpace.md), children: [
                  if (simulated) ...[const SimulatedBanner(), const SizedBox(height: AppSpace.md)],
                  const SectionLabel('Pay from'),
                  ProviderPicker(providers: available.map((p) => p.name).toList(), selected: _provider, onChanged: (p) => setState(() => _provider = p)),
                  const SizedBox(height: AppSpace.md),
                  TextFormField(
                    key: const Key('deposit.phone'),
                    controller: _phone,
                    keyboardType: TextInputType.phone,
                    decoration: InputDecoration(labelText: '${providerLabel(_provider)} number'),
                    validator: validateKenyanPhone,
                  ),
                  const SizedBox(height: AppSpace.lg),
                  const SectionLabel('Deposit value'),
                  MoneyInput(controller: _amount, wholeUnitsOnly: true, min: limits?.perTxnMin, max: limits?.perTxnMax, onChanged: (_) => setState(() {})),
                  const SizedBox(height: AppSpace.sm),
                  QuickAmounts(amounts: const [500, 1000, 2500, 5000], onSelected: (a) => setState(() => _amount.text = '$a')),
                  const SizedBox(height: AppSpace.lg),
                  if (limits != null)
                    AppCard(
                      color: AppColors.surfaceTint,
                      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                        Text('Breakdown & limits', style: AppText.titleMd),
                        const SizedBox(height: AppSpace.sm),
                        Text('Deposit fee: ${limits.feeDescription ?? 'Free'}', style: AppText.bodyMd),
                        Text('Remaining today: ${limits.remaining.format()}', style: AppText.bodyMd),
                        Text('Per transaction: ${limits.perTxnMin.format()} - ${limits.perTxnMax.format()}', style: AppText.bodySm.copyWith(color: AppColors.secondary)),
                      ]),
                    ),
                  const SizedBox(height: AppSpace.md),
                  AppCard(
                    child: Row(crossAxisAlignment: CrossAxisAlignment.start, children: [
                      const Icon(Icons.shield_outlined, color: AppColors.primary),
                      const SizedBox(width: AppSpace.md),
                      Expanded(
                        child: Text('When you continue, ${providerLabel(_provider)} will send a prompt to your phone. '
                            'Enter your ${providerLabel(_provider)} PIN only on that prompt - we will never ask for it.',
                            style: AppText.bodyMd.copyWith(color: AppColors.secondary)),
                      ),
                    ]),
                  ),
                  const SizedBox(height: AppSpace.lg),
                  if (_error != null) Padding(padding: const EdgeInsets.only(bottom: AppSpace.md), child: Text(_error!, key: const Key('deposit.error'), style: AppText.bodyMd.copyWith(color: AppColors.danger))),
                  if (_unknown)
                    Padding(padding: const EdgeInsets.only(bottom: AppSpace.md), child: UnknownOutcomeCard(onCheck: _submit, busy: _busy))
                  else
                    FilledButton.icon(
                      key: const Key('deposit.submit'),
                      onPressed: _busy ? null : _submit,
                      icon: const Icon(Icons.lock_outline),
                      label: Text(_busy ? 'Sending request…' : _continueLabel()),
                    ),
                ]),
              ),
      ),
    );
  }

  String _continueLabel() {
    try {
      final m = Money(Money.parseMinor(_amount.text.trim(), Currency.kes), Currency.kes);
      return m.isZero ? 'Continue' : 'Continue (${m.format()})';
    } catch (_) {
      return 'Continue';
    }
  }
}
