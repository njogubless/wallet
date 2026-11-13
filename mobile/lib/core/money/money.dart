/// Exact money handling for the client.
///
/// Amounts travel as decimal strings ("5000.00") and are held as integer
/// minor units. `double` is never used for money. The client only formats
/// and validates input; balances and fees are always computed by the server.
library;

class Currency {
  const Currency(this.code, this.minorUnits);
  final String code;
  final int minorUnits;

  static const kes = Currency('KES', 2);

  static Currency of(String code) => switch (code) {
        'KES' => kes,
        _ => Currency(code, 2),
      };
}

class MoneyFormatException implements Exception {
  MoneyFormatException(this.message);
  final String message;
  @override
  String toString() => message;
}

class Money implements Comparable<Money> {
  const Money(this.minor, this.currency);

  final int minor;
  final Currency currency;

  static Money zero([Currency c = Currency.kes]) => Money(0, c);

  /// Parses the API wire format `{"amount": "5000.00", "currency": "KES"}`.
  factory Money.fromJson(Map<String, dynamic> json) {
    final c = Currency.of(json['currency'] as String);
    return Money(parseMinor(json['amount'] as String, c, allowNegative: true), c);
  }

  /// Parses a decimal string into minor units. Rejects separators, signs
  /// (unless [allowNegative]), exponents and excess precision.
  static int parseMinor(String input, Currency c, {bool allowNegative = false}) {
    var s = input.trim();
    var negative = false;
    if (allowNegative && s.startsWith('-')) {
      negative = true;
      s = s.substring(1);
    }
    final match = RegExp(r'^(\d+)(?:\.(\d+))?$').firstMatch(s);
    if (match == null) throw MoneyFormatException('Enter a valid amount');
    final whole = match.group(1)!;
    final frac = match.group(2) ?? '';
    if (frac.length > c.minorUnits) {
      throw MoneyFormatException('At most ${c.minorUnits} decimal places');
    }
    if (whole.length > 13) throw MoneyFormatException('Amount is too large');
    var factor = 1;
    for (var i = 0; i < c.minorUnits; i++) {
      factor *= 10;
    }
    final fracPadded = frac.padRight(c.minorUnits, '0');
    final value = int.parse(whole) * factor + (fracPadded.isEmpty ? 0 : int.parse(fracPadded));
    return negative ? -value : value;
  }

  /// Canonical wire string, e.g. "5000.00".
  String toWire() {
    final f = _factor;
    final abs = minor.abs();
    final whole = abs ~/ f;
    final frac = (abs % f).toString().padLeft(currency.minorUnits, '0');
    final sign = minor < 0 ? '-' : '';
    return currency.minorUnits == 0 ? '$sign$whole' : '$sign$whole.$frac';
  }

  int get _factor {
    var f = 1;
    for (var i = 0; i < currency.minorUnits; i++) {
      f *= 10;
    }
    return f;
  }

  bool get isWholeMajor => minor % _factor == 0;

  /// Display form with grouping: "84,250.00" (no currency code).
  String formatNumber({bool showDecimals = true}) {
    final f = _factor;
    final abs = minor.abs();
    final whole = (abs ~/ f).toString();
    final buf = StringBuffer();
    for (var i = 0; i < whole.length; i++) {
      if (i > 0 && (whole.length - i) % 3 == 0) buf.write(',');
      buf.write(whole[i]);
    }
    if (showDecimals && currency.minorUnits > 0) {
      buf.write('.');
      buf.write((abs % f).toString().padLeft(currency.minorUnits, '0'));
    }
    return (minor < 0 ? '-' : '') + buf.toString();
  }

  /// "KES 84,250.00" with a non-breaking space, per the design system.
  String format() => '${currency.code} ${formatNumber()}';

  Money operator +(Money other) {
    _check(other);
    return Money(minor + other.minor, currency);
  }

  Money operator -(Money other) {
    _check(other);
    return Money(minor - other.minor, currency);
  }

  void _check(Money other) {
    if (other.currency.code != currency.code) {
      throw ArgumentError('Cannot combine ${currency.code} with ${other.currency.code}');
    }
  }

  bool operator <(Money other) => compareTo(other) < 0;
  bool operator >(Money other) => compareTo(other) > 0;
  bool operator <=(Money other) => compareTo(other) <= 0;
  bool operator >=(Money other) => compareTo(other) >= 0;

  bool get isZero => minor == 0;
  bool get isNegative => minor < 0;

  @override
  int compareTo(Money other) {
    _check(other);
    return minor.compareTo(other.minor);
  }

  @override
  bool operator ==(Object other) =>
      other is Money && other.minor == minor && other.currency.code == currency.code;

  @override
  int get hashCode => Object.hash(minor, currency.code);

  @override
  String toString() => format();
}
