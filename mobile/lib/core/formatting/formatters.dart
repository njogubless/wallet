import 'package:intl/intl.dart';

/// All user-facing times are shown in East Africa Time (UTC+3, no DST).
DateTime toEat(DateTime t) => t.toUtc().add(const Duration(hours: 3));

String formatDateTime(DateTime t) => '${DateFormat('d MMM yyyy, HH:mm').format(toEat(t))} EAT';
String formatTime(DateTime t) => DateFormat('HH:mm').format(toEat(t));
String formatDate(DateTime t) => DateFormat('d MMM yyyy').format(toEat(t));

/// "Today", "Yesterday" or a date, for grouping history.
String dayLabel(DateTime t, {DateTime? now}) {
  final n = toEat(now ?? DateTime.now());
  final d = toEat(t);
  final today = DateTime.utc(n.year, n.month, n.day);
  final day = DateTime.utc(d.year, d.month, d.day);
  final diff = today.difference(day).inDays;
  if (diff == 0) return 'Today • ${DateFormat('d MMM yyyy').format(d)}';
  if (diff == 1) return 'Yesterday • ${DateFormat('d MMM yyyy').format(d)}';
  return DateFormat('d MMM yyyy').format(d);
}

String providerLabel(String? p) => switch (p) {
      'mpesa' => 'M-Pesa',
      'airtel' => 'Airtel Money',
      null => '',
      _ => p,
    };
