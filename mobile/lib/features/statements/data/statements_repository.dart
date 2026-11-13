import '../../../core/networking/api_client.dart';

class StatementInfo {
  const StatementInfo({required this.id, required this.periodStart, required this.periodEnd, required this.format, required this.sizeBytes, required this.createdAt});
  final String id;
  final String periodStart;
  final String periodEnd;
  final String format;
  final int sizeBytes;
  final DateTime createdAt;

  factory StatementInfo.fromJson(Map<String, dynamic> j) => StatementInfo(
        id: j['id'] as String,
        periodStart: j['period_start'] as String,
        periodEnd: j['period_end'] as String,
        format: j['format'] as String,
        sizeBytes: (j['size_bytes'] as num).toInt(),
        createdAt: DateTime.parse(j['created_at'] as String),
      );
}

abstract class StatementsRepository {
  Future<List<StatementInfo>> list();
  Future<StatementInfo> generate({required DateTime from, required DateTime to, required String format});
  Future<List<int>> download(String id);
}

class ApiStatementsRepository implements StatementsRepository {
  ApiStatementsRepository(this.api);
  final ApiClient api;

  static String _d(DateTime d) => '${d.year.toString().padLeft(4, '0')}-${d.month.toString().padLeft(2, '0')}-${d.day.toString().padLeft(2, '0')}';

  @override
  Future<List<StatementInfo>> list() async =>
      ((await api.get('/api/v1/statements'))['data'] as List).map((e) => StatementInfo.fromJson((e as Map).cast())).toList();

  @override
  Future<StatementInfo> generate({required DateTime from, required DateTime to, required String format}) async =>
      StatementInfo.fromJson(await api.post('/api/v1/statements', body: {'from': _d(from), 'to': _d(to), 'format': format}));

  @override
  Future<List<int>> download(String id) => api.getBytes('/api/v1/statements/$id/download');
}
