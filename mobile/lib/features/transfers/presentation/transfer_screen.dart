import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../app/providers.dart';
import '../../../app/theme/app_theme.dart';
import '../../../app/theme/tokens.dart';
import '../../../core/networking/api_error.dart';
import '../../../core/widgets/app_shell.dart';
import '../../../core/widgets/common.dart';
import '../../../core/widgets/confirmation_sheet.dart';
import '../../../core/widgets/money_widgets.dart';
import '../../deposits/data/payments_repository.dart';
import '../../deposits/presentation/payment_flow.dart';
import '../../wallet/domain/models.dart';
import '../../wallet/presentation/wallet_providers.dart';

class TransferScreen extends ConsumerStatefulWidget {
  const TransferScreen({super.key});
  @override
  ConsumerState<TransferScreen> createState() => _TransferScreenState();
}

class _TransferScreenState extends ConsumerState<TransferScreen> {
  final _form = GlobalKey<FormState>();
  final _to = TextEditingController();
  final _amount = TextEditingController();
  final _note = TextEditingController();
  final _submitter = PaymentSubmitter();
  final _debounce = Debouncer();

  Recipient? _recipient;
  String? _lookupError;
  bool _looking = false;
  Quote? _quote;
  String? _error;
  bool _unknown = false;
  bool _busy = false;
  String? _lastPin;

  bool get _byAccount => RegExp(r'^\d{12}$').hasMatch(_to.text.trim());

  @override
  void dispose() {
    _debounce.dispose();
    super.dispose();
  }

  Future<void> _lookup() async {
    final v = _to.text.trim();
    if (v.isEmpty) return;
    setState(() {
      _looking = true;
      _lookupError = null;
      _recipient = null;
    });
    try {
      final r = await ref.read(paymentsRepositoryProvider).lookupRecipient(phone: _byAccount ? null : v, accountNumber: _byAccount ? v : null);
      if (mounted) setState(() => _recipient = r);
    } on ApiException catch (e) {
      if (mounted) setState(() => _lookupError = e.code == 'not_found' ? 'No wallet found for this number' : e.message);
    } finally {
      if (mounted) setState(() => _looking = false);
    }
  }

  void _refreshQuote() {
    final v = _amount.text.trim();
    if (MoneyInput.validate(v) != null) {
      setState(() => _quote = null);
      return;
    }
    _debounce(const Duration(milliseconds: 300), () async {
      try {
        final q = await ref.read(walletRepositoryProvider).quote(operation: 'transfer', amount: v);
        if (mounted && _amount.text.trim() == v) setState(() => _quote = q);
      } catch (_) {
        if (mounted) setState(() => _quote = null);
      }
    });
  }

  Future<void> _authorize() async {
    if (_busy || _recipient == null || _quote == null || !_form.currentState!.validate()) return;
    final pin = await showPinConfirmation(context,
        title: 'Authorize transfer',
        details: [('To', '${_recipient!.displayName} (${_recipient!.phoneMasked})'), ('Amount', _quote!.amount.format()), ('Fee', _quote!.fee.isZero ? 'Free' : _quote!.fee.format())],
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
    final to = _to.text.trim();
    final note = _note.text.trim();
    // PIN is excluded from the fingerprint: re-entering a PIN for the same
    // transfer must reuse the same idempotency key.
    final result = await _submitter.submit(
      {'to': to, 'amount': amount, 'note': note},
      (attempt) => ref.read(paymentsRepositoryProvider).createTransfer(attempt,
          phone: _byAccount ? null : to, accountNumber: _byAccount ? to : null, amount: amount, pin: pin, description: note),
    );
    if (!mounted) return;
    setState(() => _busy = false);
    switch (result) {
      case Submitted(:final value):
        _lastPin = null;
        invalidateMoney(ref);
        context.push('/transfer/done', extra: value);
        _reset();
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

  void _reset() => setState(() {
        _to.clear();
        _amount.clear();
        _note.clear();
        _recipient = null;
        _quote = null;
        _unknown = false;
      });

  @override
  Widget build(BuildContext context) {
    final balance = ref.watch(balanceProvider).valueOrNull;
    final limits = ref.watch(limitsProvider).valueOrNull?.of('transfer');
    return Scaffold(
      appBar: const BrandHeader(title: 'Transfer Funds'),
      body: Form(
        key: _form,
        child: ListView(padding: const EdgeInsets.all(AppSpace.md), children: [
          AppCard(
            child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
              const SectionLabel('Recipient details'),
              TextFormField(
                key: const Key('transfer.to'),
                controller: _to,
                keyboardType: TextInputType.phone,
                decoration: InputDecoration(
                  labelText: 'Phone or wallet account number',
                  suffixIcon: IconButton(tooltip: 'Find recipient', onPressed: _lookup, icon: const Icon(Icons.person_search_outlined)),
                ),
                onChanged: (_) => setState(() => _recipient = null),
                onFieldSubmitted: (_) => _lookup(),
              ),
              if (_looking) const Padding(padding: EdgeInsets.only(top: AppSpace.sm), child: LinearProgressIndicator()),
              if (_lookupError != null)
                Padding(padding: const EdgeInsets.only(top: AppSpace.sm), child: Text(_lookupError!, style: AppText.bodySm.copyWith(color: AppColors.danger))),
              if (_recipient != null)
                Padding(
                  padding: const EdgeInsets.only(top: AppSpace.md),
                  child: Row(children: [
                    CircleAvatar(backgroundColor: AppColors.surfaceHigh, child: Text(_recipient!.displayName[0], style: AppText.titleMd)),
                    const SizedBox(width: AppSpace.md),
                    Expanded(
                      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                        Text(_recipient!.displayName, key: const Key('transfer.recipient'), style: AppText.headlineSm),
                        Text(_recipient!.phoneMasked, style: AppText.bodyMd.copyWith(color: AppColors.secondary)),
                        Row(children: [
                          const Icon(Icons.verified, size: 14, color: AppColors.success),
                          const SizedBox(width: 4),
                          Text('Wallet account verified', style: AppText.labelMd.copyWith(color: AppColors.success)),
                        ]),
                      ]),
                    ),
                  ]),
                ),
            ]),
          ),
          const SizedBox(height: AppSpace.md),
          AppCard(
            child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
              SectionLabel('Amount to transfer',
                  trailing: balance == null ? null : Text('Available: ${balance.available.format()}', style: AppText.labelMd.copyWith(color: AppColors.success))),
              MoneyInput(controller: _amount, min: limits?.perTxnMin, max: limits?.perTxnMax, onChanged: (_) => _refreshQuote()),
              const SizedBox(height: AppSpace.sm),
              QuickAmounts(amounts: const [500, 1000, 5000, 10000], onSelected: (a) {
                _amount.text = '$a';
                _refreshQuote();
              }),
            ]),
          ),
          const SizedBox(height: AppSpace.md),
          AppCard(
            child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
              const SectionLabel('Payment purpose & reference'),
              TextFormField(controller: _note, maxLength: 140, decoration: const InputDecoration(hintText: 'e.g. Dinner & transport share', counterText: '')),
            ]),
          ),
          const SizedBox(height: AppSpace.md),
          if (_quote != null)
            FeeSummary(rows: [('P2P transfer fee', _quote!.fee)], totalLabel: 'Total amount deducted', total: _quote!.total),
          const SizedBox(height: AppSpace.lg),
          if (_error != null) Padding(padding: const EdgeInsets.only(bottom: AppSpace.md), child: Text(_error!, key: const Key('transfer.error'), style: AppText.bodyMd.copyWith(color: AppColors.danger))),
          if (_unknown)
            UnknownOutcomeCard(busy: _busy, onCheck: () => _lastPin == null ? _authorize() : _send(_lastPin!))
          else
            FilledButton.icon(
              key: const Key('transfer.submit'),
              onPressed: _busy || _recipient == null || _quote == null ? null : _authorize,
              icon: const Icon(Icons.lock_outline),
              label: Text(_quote == null ? 'Authorize transfer' : 'Authorize transfer of ${_quote!.total.format()}'),
            ),
          const SizedBox(height: AppSpace.xl),
        ]),
      ),
    );
  }
}

class TransferDoneScreen extends StatelessWidget {
  const TransferDoneScreen({super.key, required this.result});
  final TransferResult result;

  @override
  Widget build(BuildContext context) => Scaffold(
        body: SafeArea(
          child: ListView(padding: const EdgeInsets.all(AppSpace.lg), children: [
            const SizedBox(height: AppSpace.xl),
            const Icon(Icons.check_circle, size: 72, color: AppColors.success),
            const SizedBox(height: AppSpace.md),
            Text('Transfer sent', textAlign: TextAlign.center, style: AppText.headlineMd),
            const SizedBox(height: AppSpace.sm),
            Text('${result.amount.format()} was sent to ${result.counterparty}.', textAlign: TextAlign.center, style: AppText.bodyMd.copyWith(color: AppColors.secondary)),
            const SizedBox(height: AppSpace.lg),
            FeeSummary(rows: [('Amount', result.amount), ('Fee', result.fee)], totalLabel: 'Total deducted', total: result.total),
            const SizedBox(height: AppSpace.lg),
            FilledButton(onPressed: () => context.go('/home'), child: const Text('Done')),
            TextButton(onPressed: () => context.push('/transactions/${result.id}'), child: const Text('View receipt')),
          ]),
        ),
      );
}
