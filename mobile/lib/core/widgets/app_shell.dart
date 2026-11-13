import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../app/theme/app_theme.dart';
import '../../app/theme/tokens.dart';
import '../../features/auth/presentation/session_controller.dart';
import '../../features/wallet/presentation/wallet_providers.dart';

/// Brand header used across the main tabs.
class BrandHeader extends ConsumerWidget implements PreferredSizeWidget {
  const BrandHeader({super.key, required this.title, this.actions = const []});
  final String title;
  final List<Widget> actions;

  @override
  Size get preferredSize => const Size.fromHeight(68);

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final user = ref.watch(currentUserProvider);
    final unread = ref.watch(notificationsProvider).valueOrNull?.unread ?? 0;
    return AppBar(
      toolbarHeight: 68,
      automaticallyImplyLeading: false,
      titleSpacing: AppSpace.md,
      title: Row(children: [
        Container(
          width: 40,
          height: 40,
          decoration: BoxDecoration(color: AppColors.primary, borderRadius: BorderRadius.circular(AppRadius.control)),
          child: const Icon(Icons.shield_outlined, color: AppColors.tertiaryAccent, size: 22),
        ),
        const SizedBox(width: AppSpace.sm),
        Expanded(
          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            Text('SOVEREIGN TRUST', style: AppText.labelSm.copyWith(color: AppColors.secondary, letterSpacing: 1)),
            Text(title, style: AppText.headlineSm, overflow: TextOverflow.ellipsis),
          ]),
        ),
      ]),
      actions: [
        ...actions,
        if (user != null && user.phoneVerified)
          Container(
            margin: const EdgeInsets.only(right: 4),
            padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 4),
            decoration: BoxDecoration(color: AppColors.successBg, borderRadius: BorderRadius.circular(AppRadius.chip)),
            child: Row(mainAxisSize: MainAxisSize.min, children: [
              const Icon(Icons.verified_user_outlined, size: 14, color: AppColors.success),
              const SizedBox(width: 4),
              Text('Tier ${user.kycTier}', style: AppText.labelSm.copyWith(color: AppColors.success)),
            ]),
          ),
        IconButton(
          tooltip: unread > 0 ? 'Notifications, $unread unread' : 'Notifications',
          onPressed: () => context.push('/notifications'),
          icon: Badge(isLabelVisible: unread > 0, smallSize: 8, child: const Icon(Icons.notifications_none_rounded)),
        ),
        const SizedBox(width: 4),
      ],
    );
  }
}

class AppShell extends StatelessWidget {
  const AppShell({super.key, required this.shell});
  final StatefulNavigationShell shell;

  static const _items = [
    (Icons.home_outlined, Icons.home_rounded, 'Home'),
    (Icons.account_balance_wallet_outlined, Icons.account_balance_wallet, 'Wallet'),
    (Icons.send_outlined, Icons.send_rounded, 'Transfer'),
    (Icons.receipt_long_outlined, Icons.receipt_long, 'Activity'),
    (Icons.shield_outlined, Icons.shield, 'Profile'),
  ];

  @override
  Widget build(BuildContext context) => Scaffold(
        body: shell,
        bottomNavigationBar: DecoratedBox(
          decoration: const BoxDecoration(color: AppColors.surface, border: Border(top: BorderSide(color: AppColors.border))),
          child: SafeArea(
            top: false,
            child: SizedBox(
              height: 68,
              child: Row(children: [
                for (var i = 0; i < _items.length; i++) Expanded(child: _navItem(context, i)),
              ]),
            ),
          ),
        ),
      );

  Widget _navItem(BuildContext context, int i) {
    final (icon, active, label) = _items[i];
    final selected = shell.currentIndex == i;
    final isCenter = i == 2;
    return Semantics(
      selected: selected,
      button: true,
      label: label,
      child: InkWell(
        onTap: () => shell.goBranch(i, initialLocation: i == shell.currentIndex),
        child: Column(mainAxisAlignment: MainAxisAlignment.center, children: [
          if (isCenter)
            Container(
              width: 44,
              height: 44,
              decoration: const BoxDecoration(color: AppColors.primary, shape: BoxShape.circle, boxShadow: AppShadows.floating),
              child: const Icon(Icons.send_rounded, color: Colors.white, size: 20),
            )
          else
            Icon(selected ? active : icon, color: selected ? AppColors.primary : AppColors.muted),
          const SizedBox(height: 2),
          Text(label, style: AppText.labelMd.copyWith(color: selected ? AppColors.primary : AppColors.muted)),
        ]),
      ),
    );
  }
}
