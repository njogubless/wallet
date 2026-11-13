import '../../../core/networking/api_client.dart';
import '../domain/models.dart';

abstract class WalletRepository {
  Future<Wallet> primaryWallet();
  Future<Balance> balance(String walletId);
  Future<Limits> limits();
  Future<List<ProviderInfo>> providers();
  Future<Quote> quote({required String operation, required String amount, String? provider});
}

class ApiWalletRepository implements WalletRepository {
  ApiWalletRepository(this.api);
  final ApiClient api;

  @override
  Future<Wallet> primaryWallet() async {
    final res = await api.get('/api/v1/wallets');
    final list = (res['data'] as List).map((e) => Wallet.fromJson((e as Map).cast())).toList();
    return list.firstWhere((w) => w.currency == 'KES', orElse: () => list.first);
  }

  @override
  Future<Balance> balance(String walletId) async => Balance.fromJson(await api.get('/api/v1/wallets/$walletId/balance'));

  @override
  Future<Limits> limits() async => Limits.fromJson(await api.get('/api/v1/limits'));

  @override
  Future<List<ProviderInfo>> providers() async {
    final res = await api.get('/api/v1/meta');
    return (res['providers'] as List).map((e) => ProviderInfo.fromJson((e as Map).cast())).toList();
  }

  @override
  Future<Quote> quote({required String operation, required String amount, String? provider}) async => Quote.fromJson(await api
      .get('/api/v1/fees/quote', query: {'operation': operation, 'amount': amount, 'provider': ?provider}));
}
