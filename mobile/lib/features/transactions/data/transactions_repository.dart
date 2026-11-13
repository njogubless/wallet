import '../../../core/networking/api_client.dart';
import '../domain/models.dart';

abstract class TransactionsRepository {
  Future<ActivityPage> list(ActivityQuery query, {String? cursor, int limit = 30});
  Future<Receipt> receipt(String id);
}

class ApiTransactionsRepository implements TransactionsRepository {
  ApiTransactionsRepository(this.api);
  final ApiClient api;

  @override
  Future<ActivityPage> list(ActivityQuery query, {String? cursor, int limit = 30}) async {
    final res = await api.get('/api/v1/transactions', query: {...query.toQuery(), 'limit': '$limit', 'cursor': ?cursor});
    return ActivityPage(
      (res['data'] as List).map((e) => Activity.fromJson((e as Map).cast())).toList(),
      res['next_cursor'] as String?,
    );
  }

  @override
  Future<Receipt> receipt(String id) async => Receipt.fromJson(await api.get('/api/v1/transactions/$id'));
}
