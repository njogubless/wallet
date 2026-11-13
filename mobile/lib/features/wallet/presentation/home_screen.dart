import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../app/theme/app_theme.dart';
import '../../../app/theme/tokens.dart';
import '../../../core/money/money.dart';
import '../../../core/widgets/app_shell.dart';
import '../../../core/widgets/common.dart';
import '../../../core/widgets/money_widgets.dart';
import '../../auth/presentation/session_controller.dart';
import '../domain/models.dart';
import 'wallet_providers.dart';

class HomeScreen extends ConsumerWidget {
  const HomeScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final user = ref.watch(currentUserProvider);
    final wallet = ref.watch(primaryWalletProvider);
    final balance = ref.watch(balanceProvider);
    final recent = ref.watch(recentActivityProvider);
    final limits = ref.watch(limitsProvider);
    final providers = ref.watch(paymentProvidersProvider);
    final hidden = ref.watch(balanceHiddenProvider);
    final simulated = providers.valueOrNull?.any((p) => p.simulated) ?? false;

    return Scaffold(
      appBar: const BrandHeader(title: 'Home'),
      body: RefreshIndicator(
        onRefresh: () async {
          invalidateMoney(ref);
          await ref.read(balanceProvider.future);
        },
        child: ListView(padding: const EdgeInsets.all(AppSpace.md), children: [
          Text.rich(TextSpan(children: [
            TextSpan(text: 'Habari, ', style: AppText.bodyLg.copyWith(color: AppColors.secondary)),
            TextSpan(text: user?.firstName ?? '', style: AppText.headlineMd),
          ])),
          if (limits.valueOrNull?.of('transfer') case final t?)
            Padding(
              padding: const EdgeInsets.only(top: 4),
              child: Text('Tier ${limits.value!.kycTier} account • Daily limit ${t.dailyMax.format()}',
                  style: AppText.labelMd.copyWith(color: AppColors.secondary)),
            ),
          const SizedBox(height: AppSpace.md),
          if (simulated) ...[const SimulatedBanner(), const SizedBox(height: AppSpace.md)],
          balance.when(
            data: (b) => BalanceCard(
              label: 'Primary wallet',
              available: b.available,
              held: b.held,
              pendingInbound: b.pendingInbound,
              accountMasked: wallet.valueOrNull?.maskedAccount,
              hidden: hidden,
              onToggleHidden: () => ref.read(balanceHiddenProvider.notifier).state = !hidden,
            ),
            loading: () => const SizedBox(height: 180, child: LoadingView()),
            error: (e, _) => AppCard(child: ErrorView(error: e, onRetry: () => ref.invalidate(balanceProvider))),
          ),
          if (balance.valueOrNull case final b? when !b.pendingInbound.isZero) ...[
            const SizedBox(height: AppSpace.md),
            AppCard(
              color: AppColors.surfaceHigh,
              onTap: () => context.go('/activity'),
              child: Row(children: [
                const Icon(Icons.sync_rounded, color: AppColors.secondary),
                const SizedBox(width: AppSpace.md),
                Expanded(
                  child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                    Text('IN-FLIGHT TRANSACTION', style: AppText.labelSm.copyWith(color: AppColors.secondary)),
                    Text('Deposit of ${b.pendingInbound.format()} awaiting confirmation', style: AppText.bodyMd),
                  ]),
                ),
                const Text('Track'),
              ]),
            ),
          ],
          const SizedBox(height: AppSpace.lg),
          Row(children: [
            Expanded(child: _QuickAction(icon: Icons.add, label: 'Deposit', sub: 'M-Pesa / Airtel', onTap: () => context.push('/deposit'))),
            Expanded(child: _QuickAction(icon: Icons.north_east, label: 'Send', sub: 'Instant', primary: true, onTap: () => context.go('/transfer'))),
            Expanded(child: _QuickAction(icon: Icons.south_west, label: 'Withdraw', sub: 'To mobile', onTap: () => context.push('/withdraw'))),
            Expanded(child: _QuickAction(icon: Icons.description_outlined, label: 'Statement', sub: 'PDF / CSV', onTap: () => context.push('/statements'))),
          ]),
          const SizedBox(height: AppSpace.lg),
          Row(children: [
            Expanded(child: Text('Recent Activity', style: AppText.headlineMd)),
            TextButton(onPressed: () => context.go('/activity'), child: const Text('View all')),
          ]),
          const SizedBox(height: AppSpace.sm),
          recent.when(
            data: (items) => items.isEmpty
                ? const EmptyView(message: 'No transactions yet. Make your first deposit to get started.')
                : Column(children: [
                    for (final a in items)
                      Padding(
                        padding: const EdgeInsets.only(bottom: AppSpace.sm),
                        child: TransactionRow(a, onTap: () => context.push('/transactions/${a.id}')),
                      ),
                  ]),
            loading: () => const Padding(padding: EdgeInsets.all(AppSpace.lg), child: LoadingView()),
            error: (e, _) => ErrorView(error: e, onRetry: () => ref.invalidate(recentActivityProvider)),
          ),
          const SizedBox(height: AppSpace.xl),
        ]),
      ),
    );
  }
}

class _QuickAction extends StatelessWidget {
  const _QuickAction({required this.icon, required this.label, required this.sub, required this.onTap, this.primary = false});
  final IconData icon;
  final String label;
  final String sub;
  final VoidCallback onTap;
  final bool primary;

  @override
  Widget build(BuildContext context) => InkWell(
        borderRadius: BorderRadius.circular(AppRadius.control),
        onTap: () {
          HapticFeedback.lightImpact();
          onTap();
        },
        child: Padding(
          padding: const EdgeInsets.symmetric(vertical: AppSpace.sm),
          child: Column(children: [
            Container(
              width: 60,
              height: 60,
              decoration: BoxDecoration(
                color: primary ? AppColors.primary : AppColors.surface,
                shape: BoxShape.circle,
                border: Border.all(color: AppColors.border),
                boxShadow: AppShadows.card,
              ),
              child: Icon(icon, color: primary ? Colors.white : AppColors.primary),
            ),
            const SizedBox(height: 6),
            Text(label, style: AppText.labelLg),
            Text(sub, style: AppText.bodySm.copyWith(color: AppColors.muted), overflow: TextOverflow.ellipsis),
          ]),
        ),
      );
}

class WalletScreen extends ConsumerWidget {
  const WalletScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final wallet = ref.watch(primaryWalletProvider);
    final balance = ref.watch(balanceProvider);
    final limits = ref.watch(limitsProvider);
    return Scaffold(
      appBar: const BrandHeader(title: 'Wallet'),
      body: RefreshIndicator(
        onRefresh: () async => invalidateMoney(ref),
        child: ListView(padding: const EdgeInsets.all(AppSpace.md), children: [
          wallet.when(
            data: (w) => AppCard(
              child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                Text('ACCOUNT REFERENCE', style: AppText.labelSm.copyWith(color: AppColors.secondary)),
                Text('Individual Wallet (${w.currency})', style: AppText.headlineSm),
                const SizedBox(height: AppSpace.sm),
                Container(
                  padding: const EdgeInsets.symmetric(horizontal: AppSpace.md, vertical: AppSpace.sm),
                  decoration: BoxDecoration(color: AppColors.surfaceTint, borderRadius: BorderRadius.circular(AppRadius.control)),
                  child: Row(children: [
                    Text('ID: ', style: AppText.labelMd.copyWith(color: AppColors.muted)),
                    Expanded(child: Text(w.accountNumber, style: AppText.titleMd.copyWith(fontFeatures: tabular))),
                    TextButton.icon(
                      onPressed: () {
                        Clipboard.setData(ClipboardData(text: w.accountNumber));
                        showSnack(context, 'Account number copied');
                      },
                      icon: const Icon(Icons.copy, size: 16),
                      label: const Text('Copy'),
                    ),
                  ]),
                ),
              ]),
            ),
            loading: () => const SizedBox.shrink(),
            error: (e, _) => ErrorView(error: e),
          ),
          const SizedBox(height: AppSpace.md),
          balance.when(
            data: (b) => Container(
              padding: const EdgeInsets.all(AppSpace.lg),
              decoration: BoxDecoration(color: AppColors.primary, borderRadius: BorderRadius.circular(AppRadius.card)),
              child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                Text('TOTAL BOOK BALANCE', style: AppText.labelMd.copyWith(color: Colors.white70)),
                const SizedBox(height: AppSpace.sm),
                MoneyText(b.total, style: AppText.displayMobile.copyWith(color: Colors.white),
                    prefixStyle: AppText.currencyPrefix.copyWith(color: Colors.white70, fontSize: 18)),
                const SizedBox(height: AppSpace.md),
                Row(children: [
                  Expanded(child: _split('Available spendable', b.available)),
                  Expanded(child: _split('Held for payouts', b.held)),
                ]),
              ]),
            ),
            loading: () => const SizedBox(height: 160, child: LoadingView()),
            error: (e, _) => ErrorView(error: e, onRetry: () => ref.invalidate(balanceProvider)),
          ),
          const SizedBox(height: AppSpace.md),
          Row(children: [
            Expanded(child: _tile(Icons.add, 'Add money', () => context.push('/deposit'))),
            const SizedBox(width: AppSpace.sm),
            Expanded(child: _tile(Icons.account_balance_outlined, 'Withdraw', () => context.push('/withdraw'))),
            const SizedBox(width: AppSpace.sm),
            Expanded(child: _tile(Icons.description_outlined, 'Statements', () => context.push('/statements'))),
          ]),
          const SizedBox(height: AppSpace.md),
          limits.when(
            data: (l) => AppCard(
              child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                Row(children: [
                  Icon(l.phoneVerified ? Icons.verified_outlined : Icons.error_outline, color: l.phoneVerified ? AppColors.success : AppColors.pending),
                  const SizedBox(width: AppSpace.sm),
                  Text('Tier ${l.kycTier}', style: AppText.headlineSm),
                ]),
                Text(l.phoneVerified ? 'Phone number verified' : 'Verify your phone to unlock payments',
                    style: AppText.labelMd.copyWith(color: AppColors.secondary)),
                const SizedBox(height: AppSpace.md),
                for (final item in l.items) _LimitBar(item: item),
                Text('Higher tiers require additional identity verification by our team.',
                    style: AppText.bodySm.copyWith(color: AppColors.muted)),
              ]),
            ),
            loading: () => const SizedBox.shrink(),
            error: (e, _) => ErrorView(error: e, onRetry: () => ref.invalidate(limitsProvider)),
          ),
          const SizedBox(height: AppSpace.xl),
        ]),
      ),
    );
  }

  Widget _split(String label, Money m) => Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
        Text(label, style: AppText.bodySm.copyWith(color: Colors.white70)),
        MoneyText(m, style: AppText.titleMd.copyWith(color: Colors.white)),
      ]);

  Widget _tile(IconData icon, String label, VoidCallback onTap) => AppCard(
        onTap: onTap,
        child: Column(children: [
          CircleAvatar(backgroundColor: AppColors.surfaceTint, child: Icon(icon, color: AppColors.primary)),
          const SizedBox(height: AppSpace.sm),
          Text(label, style: AppText.labelLg, textAlign: TextAlign.center),
        ]),
      );
}

class _LimitBar extends StatelessWidget {
  const _LimitBar({required this.item});
  final LimitSummary item;

  @override
  Widget build(BuildContext context) {
    final label = switch (item.operation) { 'deposit' => 'Deposits', 'transfer' => 'Transfers', _ => 'Withdrawals' };
    return Padding(
      padding: const EdgeInsets.only(bottom: AppSpace.md),
      child: Container(
        padding: const EdgeInsets.all(AppSpace.md),
        decoration: BoxDecoration(color: AppColors.surfaceTint, borderRadius: BorderRadius.circular(AppRadius.control)),
        child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
          Row(children: [
            Expanded(child: Text('$label today', style: AppText.labelMd.copyWith(color: AppColors.secondary))),
            Text('${item.usedToday.format()} / ${item.dailyMax.formatNumber()}', style: AppText.labelMd.copyWith(fontFeatures: tabular)),
          ]),
          const SizedBox(height: 6),
          ClipRRect(
            borderRadius: BorderRadius.circular(4),
            child: LinearProgressIndicator(value: item.usedFraction, minHeight: 6, backgroundColor: AppColors.surfaceHigh, color: AppColors.primary),
          ),
          const SizedBox(height: 6),
          Text('Per transaction: up to ${item.perTxnMax.format()} • Fee: ${item.feeDescription ?? '-'}',
              style: AppText.bodySm.copyWith(color: AppColors.secondary)),
        ]),
      ),
    );
  }
}
