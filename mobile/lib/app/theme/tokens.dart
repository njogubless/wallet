import 'package:flutter/material.dart';

/// Design tokens from the Stitch "Sovereign Trust" design system.
abstract final class AppColors {
  static const primary = Color(0xFF0A2540); // deep navy
  static const primaryActive = Color(0xFF0E3A5A); // midnight teal
  static const canvas = Color(0xFFF8FAFC);
  static const surface = Color(0xFFFFFFFF);
  static const surfaceTint = Color(0xFFEFF4FF);
  static const surfaceHigh = Color(0xFFDCE9FF);
  static const border = Color(0xFFE2E8F0);
  static const onSurface = Color(0xFF0B1C30);
  static const muted = Color(0xFF64748B);
  static const secondary = Color(0xFF3C6183);

  static const success = Color(0xFF059669);
  static const successBg = Color(0xFFECFDF5);
  static const pending = Color(0xFFB45309);
  static const pendingAccent = Color(0xFFD97706);
  static const pendingBg = Color(0xFFFFFBEB);
  static const danger = Color(0xFFDC2626);
  static const dangerBg = Color(0xFFFEF2F2);
  static const unknown = Color(0xFF3C6183);
  static const unknownBg = Color(0xFFEFF4FF);
  static const outboundBg = Color(0xFFF1F5F9);
  static const tertiaryAccent = Color(0xFF68DBA9);
}

abstract final class AppRadius {
  static const card = 16.0;
  static const control = 12.0;
  static const chip = 999.0;
}

abstract final class AppSpace {
  static const xs = 4.0;
  static const sm = 8.0;
  static const md = 16.0;
  static const lg = 24.0;
  static const xl = 32.0;
}

abstract final class AppShadows {
  static const card = [
    BoxShadow(color: Color(0x0A0A2540), blurRadius: 3, offset: Offset(0, 1)),
    BoxShadow(color: Color(0x050A2540), blurRadius: 2, offset: Offset(0, 1)),
  ];
  static const floating = [BoxShadow(color: Color(0x140A2540), blurRadius: 12, offset: Offset(0, 4))];
  static const sheet = [BoxShadow(color: Color(0x1F0A2540), blurRadius: 24, offset: Offset(0, -4))];
}
