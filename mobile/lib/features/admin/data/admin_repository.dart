import '../../../core/networking/api_client.dart';

typedef Json = Map<String, dynamic>;

/// Admin API. Responses are kept as JSON maps: the console displays them and
/// never derives money values itself.
abstract class AdminRepository {
  Future<Json> dashboard();
  Future<List<Json>> payments({String? kind, String? status, bool needsReview = false});
  Future<void> recheck(String kind, String id);
  Future<Json> resolveWithdrawal(String id, {required String outcome, required String evidence, String? receipt});
  Future<List<Json>> reconItems({String status = 'open'});
  Future<Json> updateReconItem(String id, String status, String note);
  Future<Json> runReconciliation();
  Future<List<Json>> financialRequests({String? status});
  Future<Json> createFinancialRequest(Json body);
  Future<Json> decide(String id, {required bool approve, required String note});
  Future<List<Json>> searchUsers(String q);
  Future<Json> user(String id);
  Future<Json> setUserStatus(String id, String status, String reason);
  Future<List<Json>> auditEvents({String? action});
  Future<List<Json>> providerHealth();
}

class ApiAdminRepository implements AdminRepository {
  ApiAdminRepository(this.api);
  final ApiClient api;

  List<Json> _list(Json res) => (res['data'] as List).map((e) => (e as Map).cast<String, dynamic>()).toList();

  @override
  Future<Json> dashboard() => api.get('/api/v1/admin/dashboard');

  @override
  Future<List<Json>> payments({String? kind, String? status, bool needsReview = false}) async => _list(await api.get('/api/v1/admin/payments',
      query: {'kind': ?kind, 'status': ?status, if (needsReview) 'needs_review': 'true'}));

  @override
  Future<void> recheck(String kind, String id) => api.post('/api/v1/admin/${kind}s/$id/recheck');

  @override
  Future<Json> resolveWithdrawal(String id, {required String outcome, required String evidence, String? receipt}) =>
      api.post('/api/v1/admin/withdrawals/$id/resolve', body: {'outcome': outcome, 'evidence': evidence, 'provider_receipt': ?receipt});

  @override
  Future<List<Json>> reconItems({String status = 'open'}) async => _list(await api.get('/api/v1/admin/reconciliation/items', query: {'status': status}));

  @override
  Future<Json> updateReconItem(String id, String status, String note) =>
      api.post('/api/v1/admin/reconciliation/items/$id', body: {'status': status, 'note': note});

  @override
  Future<Json> runReconciliation() => api.post('/api/v1/admin/reconciliation/runs');

  @override
  Future<List<Json>> financialRequests({String? status}) async =>
      _list(await api.get('/api/v1/admin/financial-requests', query: {'status': ?status}));

  @override
  Future<Json> createFinancialRequest(Json body) => api.post('/api/v1/admin/financial-requests', body: body);

  @override
  Future<Json> decide(String id, {required bool approve, required String note}) =>
      api.post('/api/v1/admin/financial-requests/$id/${approve ? 'approve' : 'reject'}', body: {'note': note});

  @override
  Future<List<Json>> searchUsers(String q) async => _list(await api.get('/api/v1/admin/users', query: {'q': q}));

  @override
  Future<Json> user(String id) => api.get('/api/v1/admin/users/$id');

  @override
  Future<Json> setUserStatus(String id, String status, String reason) =>
      api.post('/api/v1/admin/users/$id/status', body: {'status': status, 'reason': reason});

  @override
  Future<List<Json>> auditEvents({String? action}) async =>
      _list(await api.get('/api/v1/admin/audit-events', query: {if (action != null && action.isNotEmpty) 'action': action}));

  @override
  Future<List<Json>> providerHealth() async => _list(await api.get('/api/v1/admin/providers/health'));
}
