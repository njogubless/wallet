import 'package:flutter/material.dart';

import '../../app/theme/app_theme.dart';
import '../../app/theme/tokens.dart';
import '../networking/api_error.dart';

class AppCard extends StatelessWidget {
  const AppCard({super.key, required this.child, this.padding = const EdgeInsets.all(AppSpace.md), this.color, this.onTap});
  final Widget child;
  final EdgeInsetsGeometry padding;
  final Color? color;
  final VoidCallback? onTap;

  @override
  Widget build(BuildContext context) {
    final body = Padding(padding: padding, child: child);
    return DecoratedBox(
      decoration: BoxDecoration(
        color: color ?? AppColors.surface,
        borderRadius: BorderRadius.circular(AppRadius.card),
        border: Border.all(color: AppColors.border),
        boxShadow: AppShadows.card,
      ),
      child: onTap == null
          ? body
          : Material(
              color: Colors.transparent,
              child: InkWell(borderRadius: BorderRadius.circular(AppRadius.card), onTap: onTap, child: body),
            ),
    );
  }
}

class SectionLabel extends StatelessWidget {
  const SectionLabel(this.text, {super.key, this.trailing});
  final String text;
  final Widget? trailing;

  @override
  Widget build(BuildContext context) => Padding(
        padding: const EdgeInsets.only(bottom: AppSpace.sm),
        child: Row(children: [
          Expanded(child: Text(text.toUpperCase(), style: AppText.labelSm.copyWith(color: AppColors.secondary))),
          ?trailing,
        ]),
      );
}

class LoadingView extends StatelessWidget {
  const LoadingView({super.key, this.label});
  final String? label;

  @override
  Widget build(BuildContext context) => Center(
        child: Column(mainAxisSize: MainAxisSize.min, children: [
          const SizedBox(width: 28, height: 28, child: CircularProgressIndicator(strokeWidth: 2.5)),
          if (label != null) ...[const SizedBox(height: AppSpace.md), Text(label!, style: AppText.bodyMd.copyWith(color: AppColors.muted))],
        ]),
      );
}

String errorMessage(Object error) => error is ApiException ? error.message : 'Something went wrong. Please try again.';

class ErrorView extends StatelessWidget {
  const ErrorView({super.key, required this.error, this.onRetry});
  final Object error;
  final VoidCallback? onRetry;

  @override
  Widget build(BuildContext context) => Center(
        child: Padding(
          padding: const EdgeInsets.all(AppSpace.lg),
          child: Column(mainAxisSize: MainAxisSize.min, children: [
            const Icon(Icons.cloud_off_rounded, size: 40, color: AppColors.muted),
            const SizedBox(height: AppSpace.md),
            Text(errorMessage(error), textAlign: TextAlign.center, style: AppText.bodyMd),
            if (onRetry != null) ...[
              const SizedBox(height: AppSpace.md),
              OutlinedButton(onPressed: onRetry, child: const Text('Try again')),
            ],
          ]),
        ),
      );
}

class EmptyView extends StatelessWidget {
  const EmptyView({super.key, required this.message, this.icon = Icons.receipt_long_outlined});
  final String message;
  final IconData icon;

  @override
  Widget build(BuildContext context) => Padding(
        padding: const EdgeInsets.all(AppSpace.xl),
        child: Column(children: [
          Icon(icon, size: 40, color: AppColors.muted),
          const SizedBox(height: AppSpace.sm),
          Text(message, textAlign: TextAlign.center, style: AppText.bodyMd.copyWith(color: AppColors.muted)),
        ]),
      );
}

/// Shown whenever the backend reports simulated payments, so a simulated
/// transaction can never be mistaken for a real one.
class SimulatedBanner extends StatelessWidget {
  const SimulatedBanner({super.key});

  @override
  Widget build(BuildContext context) => Container(
        width: double.infinity,
        padding: const EdgeInsets.symmetric(horizontal: AppSpace.md, vertical: AppSpace.sm),
        decoration: BoxDecoration(color: AppColors.pendingBg, borderRadius: BorderRadius.circular(AppRadius.control), border: Border.all(color: const Color(0x33D97706))),
        child: Row(children: [
          const Icon(Icons.science_outlined, size: 18, color: AppColors.pending),
          const SizedBox(width: AppSpace.sm),
          Expanded(child: Text('Simulated payments - no real money moves', style: AppText.labelMd.copyWith(color: AppColors.pending))),
        ]),
      );
}

void showSnack(BuildContext context, String message) =>
    ScaffoldMessenger.of(context)..hideCurrentSnackBar()..showSnackBar(SnackBar(content: Text(message)));
