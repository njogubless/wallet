import '../../../core/networking/api_client.dart';

class AppNotification {
  const AppNotification({required this.id, required this.category, required this.title, required this.body, required this.createdAt, required this.read});
  final String id;
  final String category;
  final String title;
  final String body;
  final DateTime createdAt;
  final bool read;

  factory AppNotification.fromJson(Map<String, dynamic> j) => AppNotification(
        id: j['id'] as String,
        category: j['category'] as String,
        title: j['title'] as String,
        body: j['body'] as String,
        createdAt: DateTime.parse(j['created_at'] as String),
        read: j['read_at'] != null,
      );
}

class NotificationFeed {
  const NotificationFeed(this.items, this.unread);
  final List<AppNotification> items;
  final int unread;
}

abstract class NotificationsRepository {
  Future<NotificationFeed> list();
  Future<void> markAllRead();
}

class ApiNotificationsRepository implements NotificationsRepository {
  ApiNotificationsRepository(this.api);
  final ApiClient api;

  @override
  Future<NotificationFeed> list() async {
    final res = await api.get('/api/v1/notifications');
    return NotificationFeed(
      (res['data'] as List).map((e) => AppNotification.fromJson((e as Map).cast())).toList(),
      (res['unread_count'] as num).toInt(),
    );
  }

  @override
  Future<void> markAllRead() => api.post('/api/v1/notifications/read', body: <String, dynamic>{});
}
