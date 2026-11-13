import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

import '../../app/theme/app_theme.dart';
import '../../app/theme/tokens.dart';
import '../money/money.dart';
import 'money_widgets.dart';

/// Bottom sheet summarising a payment and collecting the transaction PIN.
/// Returns the entered PIN, or null if cancelled. The PIN is never stored.
Future<String?> showPinConfirmation(
  BuildContext context, {
  required String title,
  required List<(String, String)> details,
  required Money total,
}) =>
    showModalBottomSheet<String>(
      context: context,
      isScrollControlled: true,
      backgroundColor: AppColors.surface,
      barrierColor: AppColors.primary.withValues(alpha: 0.4),
      shape: const RoundedRectangleBorder(borderRadius: BorderRadius.vertical(top: Radius.circular(AppRadius.card))),
      builder: (_) => _PinSheet(title: title, details: details, total: total),
    );

class _PinSheet extends StatefulWidget {
  const _PinSheet({required this.title, required this.details, required this.total});
  final String title;
  final List<(String, String)> details;
  final Money total;
  static const minLength = 4;
  static const maxLength = 6;

  @override
  State<_PinSheet> createState() => _PinSheetState();
}

class _PinSheetState extends State<_PinSheet> {
  String _pin = '';

  void _tap(String d) {
    if (_pin.length >= _PinSheet.maxLength) return;
    HapticFeedback.selectionClick();
    setState(() => _pin += d);
  }

  void _back() {
    if (_pin.isEmpty) return;
    setState(() => _pin = _pin.substring(0, _pin.length - 1));
  }

  @override
  Widget build(BuildContext context) {
    return SafeArea(
      child: Padding(
        padding: const EdgeInsets.fromLTRB(AppSpace.lg, AppSpace.md, AppSpace.lg, AppSpace.lg),
        child: Column(mainAxisSize: MainAxisSize.min, children: [
          Container(width: 40, height: 4, decoration: BoxDecoration(color: AppColors.border, borderRadius: BorderRadius.circular(2))),
          const SizedBox(height: AppSpace.md),
          Text(widget.title, style: AppText.headlineSm),
          const SizedBox(height: AppSpace.md),
          for (final (k, v) in widget.details)
            Padding(
              padding: const EdgeInsets.symmetric(vertical: 3),
              child: Row(children: [
                Expanded(child: Text(k, style: AppText.bodyMd.copyWith(color: AppColors.muted))),
                Flexible(child: Text(v, style: AppText.labelLg, textAlign: TextAlign.end)),
              ]),
            ),
          const Divider(height: AppSpace.lg),
          Row(children: [
            Expanded(child: Text('Total', style: AppText.titleMd)),
            MoneyText(widget.total, key: const Key('confirm.total'), style: AppText.headlineSm),
          ]),
          const SizedBox(height: AppSpace.lg),
          Text('Enter your transaction PIN', style: AppText.labelLg.copyWith(color: AppColors.secondary)),
          const SizedBox(height: AppSpace.md),
          Semantics(
            label: '${_pin.length} digits entered',
            child: Row(mainAxisAlignment: MainAxisAlignment.center, children: [
              for (var i = 0; i < (_pin.length > _PinSheet.minLength ? _PinSheet.maxLength : _PinSheet.minLength); i++)
                AnimatedContainer(
                  duration: const Duration(milliseconds: 120),
                  margin: const EdgeInsets.symmetric(horizontal: 8),
                  width: 14,
                  height: 14,
                  decoration: BoxDecoration(
                    shape: BoxShape.circle,
                    color: i < _pin.length ? AppColors.primary : Colors.transparent,
                    border: Border.all(color: i < _pin.length ? AppColors.primary : AppColors.border, width: 2),
                  ),
                ),
            ]),
          ),
          const SizedBox(height: AppSpace.lg),
          _keypad(),
          const SizedBox(height: AppSpace.md),
          FilledButton.icon(
            key: const Key('pin.submit'),
            onPressed: _pin.length >= _PinSheet.minLength ? () => Navigator.of(context).pop(_pin) : null,
            icon: const Icon(Icons.lock_outline),
            label: const Text('Authorize'),
          ),
        ]),
      ),
    );
  }

  Widget _keypad() {
    Widget key(String label, {VoidCallback? onTap, Widget? child}) => SizedBox(
          width: 72,
          height: 64,
          child: TextButton(
            key: Key('pin.$label'),
            onPressed: onTap,
            style: TextButton.styleFrom(shape: const CircleBorder()),
            child: child ?? Text(label, style: AppText.headlineMd.copyWith(fontFeatures: tabular)),
          ),
        );
    final rows = [
      ['1', '2', '3'],
      ['4', '5', '6'],
      ['7', '8', '9'],
    ];
    return Column(children: [
      for (final r in rows)
        Row(mainAxisAlignment: MainAxisAlignment.spaceEvenly, children: [for (final d in r) key(d, onTap: () => _tap(d))]),
      Row(mainAxisAlignment: MainAxisAlignment.spaceEvenly, children: [
        key('cancel', onTap: () => Navigator.of(context).pop(), child: const Icon(Icons.close)),
        key('0', onTap: () => _tap('0')),
        key('back', onTap: _back, child: const Icon(Icons.backspace_outlined)),
      ]),
    ]);
  }
}
