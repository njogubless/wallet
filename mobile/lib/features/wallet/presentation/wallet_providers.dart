import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../app/providers.dart';
import '../../notifications/data/notifications_repository.dart';
import '../../transactions/domain/models.dart';
import '../domain/models.dart';

final primaryWalletProvider = FutureProvider<Wallet>((ref) => ref.watch(walletRepositoryProvider).primaryWallet());

final balanceProvider = FutureProvider<Balance>((ref) async {
  final w = await ref.watch(primaryWalletProvider.future);
  return ref.watch(walletRepositoryProvider).balance(w.id);
});

final limitsProvider = FutureProvider<Limits>((ref) => ref.watch(walletRepositoryProvider).limits());

final paymentProvidersProvider = FutureProvider<List<ProviderInfo>>((ref) => ref.watch(walletRepositoryProvider).providers());

final recentActivityProvider = FutureProvider<List<Activity>>((ref) async {
  final page = await ref.watch(transactionsRepositoryProvider).list(const ActivityQuery(), limit: 5);
  return page.items;
});

final notificationsProvider = FutureProvider<NotificationFeed>((ref) => ref.watch(notificationsRepositoryProvider).list());

final balanceHiddenProvider = StateProvider<bool>((_) => false);

/// Refresh everything that depends on balances after a money movement.
void invalidateMoney(WidgetRef ref) {
  ref.invalidate(balanceProvider);
  ref.invalidate(recentActivityProvider);
  ref.invalidate(limitsProvider);
  ref.invalidate(notificationsProvider);
}
