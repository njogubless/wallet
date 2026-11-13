import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../core/config/app_config.dart';
import '../core/networking/api_client.dart';
import '../core/security/token_store.dart';
import '../features/admin/data/admin_repository.dart';
import '../features/auth/data/auth_repository.dart';
import '../features/deposits/data/payments_repository.dart';
import '../features/notifications/data/notifications_repository.dart';
import '../features/statements/data/statements_repository.dart';
import '../features/transactions/data/transactions_repository.dart';
import '../features/wallet/data/wallet_repository.dart';

final appConfigProvider = Provider<AppConfig>((_) => AppConfig.fromEnvironment);
final tokenStoreProvider = Provider<TokenStore>((_) => SecureTokenStore());

final apiClientProvider = Provider<ApiClient>(
  (ref) => ApiClient(baseUrl: ref.watch(appConfigProvider).apiBaseUrl, tokens: ref.watch(tokenStoreProvider)),
);

final authRepositoryProvider = Provider<AuthRepository>((ref) => ApiAuthRepository(ref.watch(apiClientProvider)));
final walletRepositoryProvider = Provider<WalletRepository>((ref) => ApiWalletRepository(ref.watch(apiClientProvider)));
final transactionsRepositoryProvider =
    Provider<TransactionsRepository>((ref) => ApiTransactionsRepository(ref.watch(apiClientProvider)));
final paymentsRepositoryProvider = Provider<PaymentsRepository>((ref) => ApiPaymentsRepository(ref.watch(apiClientProvider)));
final statementsRepositoryProvider =
    Provider<StatementsRepository>((ref) => ApiStatementsRepository(ref.watch(apiClientProvider)));
final notificationsRepositoryProvider =
    Provider<NotificationsRepository>((ref) => ApiNotificationsRepository(ref.watch(apiClientProvider)));
final adminRepositoryProvider = Provider<AdminRepository>((ref) => ApiAdminRepository(ref.watch(apiClientProvider)));
