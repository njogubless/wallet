class AppUser {
  const AppUser({
    required this.id,
    required this.phone,
    required this.fullName,
    required this.role,
    required this.status,
    required this.kycTier,
    required this.phoneVerified,
    required this.hasPin,
    this.email,
  });

  final String id;
  final String phone;
  final String? email;
  final String fullName;
  final String role;
  final String status;
  final int kycTier;
  final bool phoneVerified;
  final bool hasPin;

  bool get isStaff => role == 'admin' || role == 'support';
  bool get isAdmin => role == 'admin';
  bool get needsPhoneVerification => status == 'pending_verification' || !phoneVerified;
  String get firstName => fullName.trim().split(RegExp(r'\s+')).first;

  factory AppUser.fromJson(Map<String, dynamic> j) => AppUser(
        id: j['id'] as String,
        phone: j['phone'] as String,
        email: j['email'] as String?,
        fullName: j['full_name'] as String,
        role: j['role'] as String,
        status: j['status'] as String,
        kycTier: (j['kyc_tier'] as num).toInt(),
        phoneVerified: j['phone_verified_at'] != null,
        hasPin: j['has_pin'] as bool? ?? false,
      );
}
