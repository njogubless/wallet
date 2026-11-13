import '../../../core/money/money.dart';

/// Customer-facing state. `unknown` = outcome not yet confirmed (not failed).
enum DisplayStatus {
  pending,
  succeeded,
  failed,
  reversed,
  unknown;

  static DisplayStatus parse(String? s) => switch (s) {
        'succeeded' => succeeded,
        'failed' => failed,
        'reversed' => reversed,
        'pending' => pending,
        _ => unknown,
      };

  bool get isTerminal => this == succeeded || this == failed || this == reversed;
}

class Activity {
  const Activity({
    required this.id,
    required this.type,
    required this.direction,
    required this.amount,
    required this.fee,
    required this.status,
    required this.displayStatus,
    required this.createdAt,
    this.provider,
    this.counterparty,
    this.counterpartyDetail,
    this.reference,
    this.description = '',
    this.failureReason,
    this.completedAt,
  });

  final String id;
  final String type; // deposit | withdrawal | transfer_in | transfer_out | adjustment
  final String direction; // credit | debit
  final Money amount;
  final Money fee;
  final String status;
  final DisplayStatus displayStatus;
  final String? provider;
  final String? counterparty;
  final String? counterpartyDetail;
  final String? reference;
  final String description;
  final String? failureReason;
  final DateTime createdAt;
  final DateTime? completedAt;

  bool get isCredit => direction == 'credit';

  factory Activity.fromJson(Map<String, dynamic> j) => Activity(
        id: j['id'] as String,
        type: j['type'] as String,
        direction: j['direction'] as String,
        amount: Money.fromJson((j['amount'] as Map).cast()),
        fee: Money.fromJson((j['fee'] as Map).cast()),
        status: j['status'] as String,
        displayStatus: DisplayStatus.parse(j['display_status'] as String?),
        provider: j['provider'] as String?,
        counterparty: j['counterparty'] as String?,
        counterpartyDetail: j['counterparty_detail'] as String?,
        reference: j['reference'] as String?,
        description: (j['description'] as String?) ?? '',
        failureReason: j['failure_reason'] as String?,
        createdAt: DateTime.parse(j['created_at'] as String),
        completedAt: j['completed_at'] == null ? null : DateTime.parse(j['completed_at'] as String),
      );
}

class ActivityPage {
  const ActivityPage(this.items, this.nextCursor);
  final List<Activity> items;
  final String? nextCursor;
}

class ActivityQuery {
  const ActivityQuery({this.type, this.status, this.search = '', this.from, this.to});
  final String? type; // deposit | withdrawal | transfer | adjustment
  final String? status; // pending | succeeded | failed | reversed
  final String search;
  final DateTime? from;
  final DateTime? to;

  ActivityQuery copyWith({String? type, String? status, String? search, bool clearType = false, bool clearStatus = false}) =>
      ActivityQuery(
        type: clearType ? null : (type ?? this.type),
        status: clearStatus ? null : (status ?? this.status),
        search: search ?? this.search,
        from: from,
        to: to,
      );

  Map<String, dynamic> toQuery() => {
        if (type != null) 'type': type,
        if (status != null) 'status': status,
        if (search.trim().isNotEmpty) 'q': search.trim(),
        if (from != null) 'from': from!.toUtc().toIso8601String(),
        if (to != null) 'to': to!.toUtc().toIso8601String(),
      };

  @override
  bool operator ==(Object other) =>
      other is ActivityQuery &&
      other.type == type &&
      other.status == status &&
      other.search == search &&
      other.from == from &&
      other.to == to;
  @override
  int get hashCode => Object.hash(type, status, search, from, to);
}

class Receipt {
  const Receipt({required this.activity, required this.receiptNumber, required this.walletAccount, this.total, this.outcomeUncertain = false, this.environment});
  final Activity activity;
  final String receiptNumber;
  final String walletAccount;
  final Money? total;
  final bool outcomeUncertain;
  final String? environment;

  factory Receipt.fromJson(Map<String, dynamic> j) {
    final w = j['withdrawal'] as Map?;
    final t = j['transfer'] as Map?;
    final d = j['deposit'] as Map?;
    final total = w?['total'] ?? t?['total'];
    return Receipt(
      activity: Activity.fromJson((j['transaction'] as Map).cast()),
      receiptNumber: j['receipt_number'] as String,
      walletAccount: j['wallet_account'] as String,
      total: total == null ? null : Money.fromJson((total as Map).cast()),
      outcomeUncertain: (w?['outcome_uncertain'] as bool?) ?? false,
      environment: (d?['provider_environment'] ?? w?['provider_environment']) as String?,
    );
  }
}
