import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../app/providers.dart';
import '../../../app/theme/app_theme.dart';
import '../../../app/theme/tokens.dart';
import '../../../core/formatting/formatters.dart';
import '../../../core/widgets/common.dart';
import '../../wallet/presentation/wallet_providers.dart';

class NotificationsScreen extends ConsumerWidget {
  const NotificationsScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final feed = ref.watch(notificationsProvider);
    return Scaffold(
      appBar: AppBar(title: const Text('Notifications'), actions: [
        TextButton(
          onPressed: () async {
            await ref.read(notificationsRepositoryProvider).markAllRead();
            ref.invalidate(notificationsProvider);
          },
          child: const Text('Mark all read'),
        ),
      ]),
      body: RefreshIndicator(
        onRefresh: () async => ref.invalidate(notificationsProvider),
        child: feed.when(
          loading: () => const LoadingView(),
          error: (e, _) => ErrorView(error: e, onRetry: () => ref.invalidate(notificationsProvider)),
          data: (f) => f.items.isEmpty
              ? ListView(children: const [EmptyView(message: 'You are all caught up.', icon: Icons.notifications_none)])
              : ListView.separated(
                  padding: const EdgeInsets.all(AppSpace.md),
                  itemCount: f.items.length,
                  separatorBuilder: (_, _) => const SizedBox(height: AppSpace.sm),
                  itemBuilder: (_, i) {
                    final n = f.items[i];
                    final icon = switch (n.category) {
                      'security' => Icons.shield_outlined,
                      'account' => Icons.person_outline,
                      _ => Icons.swap_horiz,
                    };
                    return AppCard(
                      color: n.read ? null : AppColors.surfaceTint,
                      child: Row(crossAxisAlignment: CrossAxisAlignment.start, children: [
                        CircleAvatar(backgroundColor: n.category == 'security' ? AppColors.pendingBg : AppColors.surfaceHigh, child: Icon(icon, color: AppColors.primary)),
                        const SizedBox(width: AppSpace.md),
                        Expanded(
                          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                            Text(n.title, style: AppText.titleMd),
                            const SizedBox(height: 2),
                            Text(n.body, style: AppText.bodyMd.copyWith(color: AppColors.secondary)),
                            const SizedBox(height: 4),
                            Text(formatDateTime(n.createdAt), style: AppText.bodySm.copyWith(color: AppColors.muted)),
                          ]),
                        ),
                      ]),
                    );
                  },
                ),
        ),
      ),
    );
  }
}
