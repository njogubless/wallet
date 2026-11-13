import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

import '../../app/theme/app_theme.dart';
import '../../app/theme/tokens.dart';
import '../../features/transactions/domain/models.dart';
import '../formatting/formatters.dart';
import '../money/money.dart';
import 'common.dart';

/// "KES" prefix + tabular digits, per the design system.
class MoneyText extends StatelessWidget {
  const MoneyText(this.money, {super.key, this.style, this.prefixStyle, this.sign = '', this.strike = false});
  final Money money;
  final TextStyle? style;
  final TextStyle? prefixStyle;
  final String sign;
  final bool strike;

  @override
  Widget build(BuildContext context) {
    final base = (style ?? AppText.titleMd).copyWith(fontFeatures: tabular, decoration: strike ? TextDecoration.lineThrough : null);
    return Text.rich(
      TextSpan(children: [
        TextSpan(text: '$sign${money.currency.code} ', style: prefixStyle ?? base.copyWith(fontSize: (base.fontSize ?? 16) * 0.8, fontWeight: FontWeight.w500)),
        TextSpan(text: money.formatNumber(), style: base),
      ]),
      semanticsLabel: '$sign${money.currency.code} ${money.formatNumber()}',
      maxLines: 1,
      overflow: TextOverflow.ellipsis,
    );
  }
}

class BalanceCard extends StatelessWidget {
  const BalanceCard({
    super.key,
    required this.label,
    required this.available,
    required this.held,
    this.accountMasked,
    this.hidden = false,
    this.onToggleHidden,
    this.pendingInbound,
  });

  final String label;
  final Money available;
  final Money held;
  final Money? pendingInbound;
  final String? accountMasked;
  final bool hidden;
  final VoidCallback? onToggleHidden;

  @override
  Widget build(BuildContext context) {
    const white = Colors.white;
    final big = MediaQuery.sizeOf(context).width <= 390 ? AppText.displayMobile : AppText.displayLg;
    return Container(
      decoration: BoxDecoration(color: AppColors.primary, borderRadius: BorderRadius.circular(AppRadius.card), boxShadow: AppShadows.floating),
      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
        Padding(
          padding: const EdgeInsets.fromLTRB(AppSpace.lg, AppSpace.lg, AppSpace.md, AppSpace.md),
          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            Row(children: [
              Expanded(child: Text(label.toUpperCase(), style: AppText.labelMd.copyWith(color: white.withValues(alpha: 0.85)))),
              if (accountMasked != null)
                Container(
                  padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 6),
                  decoration: BoxDecoration(color: white.withValues(alpha: 0.1), borderRadius: BorderRadius.circular(AppRadius.chip)),
                  child: Text('Acc: $accountMasked', style: AppText.labelMd.copyWith(color: white)),
                ),
            ]),
            const SizedBox(height: AppSpace.md),
            Row(crossAxisAlignment: CrossAxisAlignment.center, children: [
              Expanded(
                child: hidden
                    ? Text('KES ••••••', style: big.copyWith(color: white))
                    : FittedBox(
                        alignment: Alignment.centerLeft,
                        fit: BoxFit.scaleDown,
                        child: MoneyText(available, key: const Key('balance.available'), style: big.copyWith(color: white),
                            prefixStyle: AppText.currencyPrefix.copyWith(color: white.withValues(alpha: 0.75), fontSize: 18)),
                      ),
              ),
              if (onToggleHidden != null)
                IconButton(
                  tooltip: hidden ? 'Show balance' : 'Hide balance',
                  onPressed: onToggleHidden,
                  style: IconButton.styleFrom(backgroundColor: white.withValues(alpha: 0.1)),
                  icon: Icon(hidden ? Icons.visibility_off_outlined : Icons.visibility_outlined, color: white),
                ),
            ]),
          ]),
        ),
        Container(
          padding: const EdgeInsets.symmetric(horizontal: AppSpace.lg, vertical: 14),
          decoration: BoxDecoration(
            color: Colors.black.withValues(alpha: 0.18),
            borderRadius: const BorderRadius.vertical(bottom: Radius.circular(AppRadius.card)),
          ),
          child: Wrap(spacing: AppSpace.lg, runSpacing: AppSpace.xs, children: [
            _meta(Icons.check_circle_outline, 'Avail', hidden ? '••••' : available.format(), AppColors.tertiaryAccent),
            _meta(Icons.hourglass_bottom_rounded, 'Hold', hidden ? '••••' : held.format(), white),
            if (pendingInbound != null && !pendingInbound!.isZero)
              _meta(Icons.sync_rounded, 'Incoming', hidden ? '••••' : pendingInbound!.format(), white),
          ]),
        ),
      ]),
    );
  }

  Widget _meta(IconData icon, String label, String value, Color iconColor) => Row(mainAxisSize: MainAxisSize.min, children: [
        Icon(icon, size: 16, color: iconColor),
        const SizedBox(width: 6),
        Text('$label: ', style: AppText.bodySm.copyWith(color: Colors.white70)),
        Text(value, style: AppText.labelMd.copyWith(color: Colors.white, fontFeatures: tabular)),
      ]);
}

class StatusChip extends StatelessWidget {
  const StatusChip(this.status, {super.key, this.label});
  final DisplayStatus status;
  final String? label;

  static (Color, Color, IconData, String) styleOf(DisplayStatus s) => switch (s) {
        DisplayStatus.succeeded => (AppColors.successBg, AppColors.success, Icons.check_circle_outline, 'Successful'),
        DisplayStatus.pending => (AppColors.pendingBg, AppColors.pending, Icons.schedule, 'Pending'),
        DisplayStatus.failed => (AppColors.dangerBg, AppColors.danger, Icons.block, 'Failed'),
        DisplayStatus.reversed => (AppColors.outboundBg, AppColors.muted, Icons.undo, 'Reversed'),
        DisplayStatus.unknown => (AppColors.unknownBg, AppColors.unknown, Icons.help_outline, 'Confirming'),
      };

  @override
  Widget build(BuildContext context) {
    final (bg, fg, icon, text) = styleOf(status);
    return Container(
      height: 24,
      padding: const EdgeInsets.symmetric(horizontal: 8),
      decoration: BoxDecoration(color: bg, borderRadius: BorderRadius.circular(AppRadius.chip), border: Border.all(color: fg.withValues(alpha: 0.2))),
      child: Row(mainAxisSize: MainAxisSize.min, children: [
        Icon(icon, size: 13, color: fg),
        const SizedBox(width: 4),
        Text(label ?? text, style: AppText.labelSm.copyWith(color: fg)),
      ]),
    );
  }
}

String activityTitle(Activity a) => switch (a.type) {
      'deposit' => '${providerLabel(a.provider)} Top Up',
      'withdrawal' => 'Withdraw to ${providerLabel(a.provider)}',
      'transfer_in' || 'transfer_out' => a.counterparty ?? 'Wallet transfer',
      'adjustment' => 'Account adjustment',
      _ => a.type,
    };

String activitySubtitle(Activity a) => switch (a.type) {
      'deposit' => 'Mobile money deposit',
      'withdrawal' => 'Mobile money payout',
      'transfer_in' => 'Transfer received',
      'transfer_out' => a.description.isNotEmpty ? a.description : 'Transfer sent',
      'adjustment' => a.description.replaceAll('_', ' '),
      _ => '',
    };

IconData activityIcon(Activity a) => switch (a.type) {
      'deposit' => Icons.south_west_rounded,
      'withdrawal' => Icons.account_balance_outlined,
      'transfer_in' => Icons.south_rounded,
      'transfer_out' => Icons.north_east_rounded,
      _ => Icons.tune_rounded,
    };

class TransactionRow extends StatelessWidget {
  const TransactionRow(this.activity, {super.key, this.onTap});
  final Activity activity;
  final VoidCallback? onTap;

  @override
  Widget build(BuildContext context) {
    final a = activity;
    final failed = a.displayStatus == DisplayStatus.failed;
    final iconBg = failed ? AppColors.dangerBg : (a.isCredit ? AppColors.successBg : AppColors.outboundBg);
    final iconFg = failed ? AppColors.danger : (a.isCredit ? AppColors.success : AppColors.primary);
    final amountColor = a.isCredit && a.displayStatus == DisplayStatus.succeeded ? AppColors.success : AppColors.primary;
    return AppCard(
      onTap: onTap,
      child: Row(children: [
        CircleAvatar(radius: 20, backgroundColor: iconBg, child: Icon(activityIcon(a), color: iconFg, size: 20)),
        const SizedBox(width: AppSpace.md),
        Expanded(
          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            Text(activityTitle(a), style: AppText.titleMd, maxLines: 1, overflow: TextOverflow.ellipsis),
            const SizedBox(height: 2),
            Text('${formatTime(a.createdAt)} • ${activitySubtitle(a)}',
                style: AppText.bodySm.copyWith(color: AppColors.muted), maxLines: 1, overflow: TextOverflow.ellipsis),
          ]),
        ),
        const SizedBox(width: AppSpace.sm),
        Column(crossAxisAlignment: CrossAxisAlignment.end, children: [
          MoneyText(a.amount, sign: a.isCredit ? '+' : '-', strike: failed, style: AppText.titleMd.copyWith(color: amountColor)),
          const SizedBox(height: 4),
          StatusChip(a.displayStatus),
        ]),
      ]),
    );
  }
}

/// Amount entry. Validates format/precision locally for usability only; the
/// server re-validates and enforces limits.
class MoneyInput extends StatelessWidget {
  const MoneyInput({super.key, required this.controller, this.currency = Currency.kes, this.wholeUnitsOnly = false, this.min, this.max, this.label = 'Amount', this.onChanged});
  final TextEditingController controller;
  final Currency currency;
  final bool wholeUnitsOnly;
  final Money? min;
  final Money? max;
  final String label;
  final ValueChanged<String>? onChanged;

  static String? validate(String? value, {Currency currency = Currency.kes, bool wholeUnitsOnly = false, Money? min, Money? max}) {
    final v = (value ?? '').replaceAll(',', '').trim();
    if (v.isEmpty) return 'Enter an amount';
    final int minor;
    try {
      minor = Money.parseMinor(v, currency);
    } on MoneyFormatException catch (e) {
      return e.message;
    }
    final m = Money(minor, currency);
    if (m.isZero) return 'Amount must be greater than zero';
    if (wholeUnitsOnly && !m.isWholeMajor) return 'Enter whole shillings only';
    if (min != null && m < min) return 'Minimum is ${min.format()}';
    if (max != null && m > max) return 'Maximum is ${max.format()}';
    return null;
  }

  @override
  Widget build(BuildContext context) => TextFormField(
        key: const Key('money.input'),
        controller: controller,
        keyboardType: TextInputType.numberWithOptions(decimal: !wholeUnitsOnly),
        inputFormatters: [FilteringTextInputFormatter.allow(RegExp(wholeUnitsOnly ? r'[0-9]' : r'[0-9.]'))],
        style: AppText.headlineLg.copyWith(fontFeatures: tabular),
        decoration: InputDecoration(
          labelText: label,
          prefixText: '${currency.code}  ',
          prefixStyle: AppText.currencyPrefix.copyWith(fontSize: 18),
        ),
        validator: (v) => validate(v, currency: currency, wholeUnitsOnly: wholeUnitsOnly, min: min, max: max),
        onChanged: onChanged,
        autovalidateMode: AutovalidateMode.onUserInteraction,
      );
}

class QuickAmounts extends StatelessWidget {
  const QuickAmounts({super.key, required this.amounts, required this.onSelected});
  final List<int> amounts;
  final ValueChanged<int> onSelected;

  @override
  Widget build(BuildContext context) => Row(
        children: [
          for (final a in amounts)
            Expanded(
              child: Padding(
                padding: const EdgeInsets.symmetric(horizontal: 3),
                child: TextButton(
                  style: TextButton.styleFrom(backgroundColor: AppColors.surfaceTint, shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(AppRadius.control))),
                  onPressed: () => onSelected(a),
                  child: Text(Money(a * 100, Currency.kes).formatNumber(showDecimals: false), style: AppText.labelLg.copyWith(fontFeatures: tabular)),
                ),
              ),
            ),
        ],
      );
}

class FeeSummary extends StatelessWidget {
  const FeeSummary({super.key, required this.rows, required this.totalLabel, required this.total});
  final List<(String, Money)> rows;
  final String totalLabel;
  final Money total;

  @override
  Widget build(BuildContext context) => AppCard(
        color: AppColors.surfaceTint,
        child: Column(children: [
          for (final (label, value) in rows)
            Padding(
              padding: const EdgeInsets.symmetric(vertical: 4),
              child: Row(children: [
                Expanded(child: Text(label, style: AppText.bodyMd.copyWith(color: AppColors.secondary))),
                value.isZero
                    ? Text('Free', style: AppText.labelLg.copyWith(color: AppColors.success))
                    : MoneyText(value, style: AppText.bodyMd.copyWith(fontWeight: FontWeight.w600)),
              ]),
            ),
          const Padding(padding: EdgeInsets.symmetric(vertical: AppSpace.sm), child: Divider()),
          Row(children: [
            Expanded(child: Text(totalLabel, style: AppText.headlineSm)),
            MoneyText(total, key: const Key('fee.total'), style: AppText.headlineSm),
          ]),
        ]),
      );
}
