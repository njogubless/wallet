import '../../../core/money/money.dart';

class Wallet {
  const Wallet({required this.id, required this.accountNumber, required this.currency, required this.status});
  final String id;
  final String accountNumber;
  final String currency;
  final String status;

  factory Wallet.fromJson(Map<String, dynamic> j) => Wallet(
        id: j['id'] as String,
        accountNumber: j['account_number'] as String,
        currency: j['currency'] as String,
        status: j['status'] as String,
      );

  String get maskedAccount => '•••• ${accountNumber.substring(accountNumber.length - 4)}';
}

/// Server-authoritative balances. The client never computes these.
class Balance {
  const Balance({required this.available, required this.held, required this.total, required this.pendingInbound, required this.asOf});
  final Money available;
  final Money held;
  final Money total;
  final Money pendingInbound;
  final DateTime asOf;

  factory Balance.fromJson(Map<String, dynamic> j) => Balance(
        available: Money.fromJson((j['available'] as Map).cast()),
        held: Money.fromJson((j['held'] as Map).cast()),
        total: Money.fromJson((j['total'] as Map).cast()),
        pendingInbound: Money.fromJson((j['pending_inbound'] as Map).cast()),
        asOf: DateTime.parse(j['as_of'] as String),
      );
}

class LimitSummary {
  const LimitSummary({required this.operation, required this.perTxnMin, required this.perTxnMax, required this.dailyMax, required this.usedToday, required this.remaining, this.feeDescription});
  final String operation;
  final Money perTxnMin;
  final Money perTxnMax;
  final Money dailyMax;
  final Money usedToday;
  final Money remaining;
  final String? feeDescription;

  double get usedFraction => dailyMax.minor == 0 ? 0 : (usedToday.minor / dailyMax.minor).clamp(0, 1).toDouble();

  factory LimitSummary.fromJson(Map<String, dynamic> j) {
    final c = Currency.of(j['currency'] as String);
    Money m(String k) => Money(Money.parseMinor(j[k] as String, c), c);
    String? fee;
    final f = j['fee'] as Map?;
    if (f != null) {
      final flat = Money(Money.parseMinor(f['flat_fee'] as String, c), c);
      final bps = (f['rate_bps'] as num).toInt();
      final parts = <String>[];
      if (!flat.isZero) parts.add(flat.format());
      if (bps > 0) parts.add('${(bps / 100).toStringAsFixed(bps % 100 == 0 ? 0 : 2)}%');
      fee = parts.isEmpty ? 'Free' : parts.join(' + ');
    }
    return LimitSummary(
      operation: j['operation'] as String,
      perTxnMin: m('per_transaction_min'),
      perTxnMax: m('per_transaction_max'),
      dailyMax: m('daily_max'),
      usedToday: m('used_today'),
      remaining: m('remaining_today'),
      feeDescription: fee,
    );
  }
}

class Limits {
  const Limits({required this.kycTier, required this.items, required this.phoneVerified});
  final int kycTier;
  final List<LimitSummary> items;
  final bool phoneVerified;

  LimitSummary? of(String op) {
    for (final i in items) {
      if (i.operation == op) return i;
    }
    return null;
  }

  factory Limits.fromJson(Map<String, dynamic> j) => Limits(
        kycTier: (j['kyc_tier'] as num).toInt(),
        items: (j['limits'] as List).map((e) => LimitSummary.fromJson((e as Map).cast())).toList(),
        phoneVerified: ((j['verification'] as Map?)?['phone_verified'] as bool?) ?? false,
      );
}

class ProviderInfo {
  const ProviderInfo({required this.name, required this.environment, required this.collection, required this.payout});
  final String name;
  final String environment;
  final bool collection;
  final bool payout;
  bool get simulated => environment == 'simulated';

  factory ProviderInfo.fromJson(Map<String, dynamic> j) {
    final caps = (j['capabilities'] as Map).cast<String, dynamic>();
    return ProviderInfo(
      name: j['name'] as String,
      environment: j['environment'] as String,
      collection: caps['collection'] as bool? ?? false,
      payout: caps['payout'] as bool? ?? false,
    );
  }
}

class Quote {
  const Quote({required this.amount, required this.fee, required this.total});
  final Money amount;
  final Money fee;
  final Money total;

  factory Quote.fromJson(Map<String, dynamic> j) => Quote(
        amount: Money.fromJson((j['amount'] as Map).cast()),
        fee: Money.fromJson((j['fee'] as Map).cast()),
        total: Money.fromJson((j['total'] as Map).cast()),
      );
}
