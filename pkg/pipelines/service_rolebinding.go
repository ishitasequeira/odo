package pipelines

import (
	corev1 "k8s.io/api/core/v1"
	v1rbac "k8s.io/api/rbac/v1"
)

// createServiceAccount creates a ServiceAccount given saName and secretName
func createServiceAccount(saName, secretName string) corev1.ServiceAccount {
	return corev1.ServiceAccount{
		TypeMeta:   createTypeMeta("ServiceAccount", "v1"),
		ObjectMeta: createObjectMeta(saName),
		Secrets: []corev1.ObjectReference{
			corev1.ObjectReference{Name: secretName},
		},
	}
}

// createRoleBinding creates a RoleBinding given name, sa, roleKind, and roleName
func createRoleBinding(name string, sa *corev1.ServiceAccount, roleKind, roleName string) v1rbac.RoleBinding {
	return v1rbac.RoleBinding{
		TypeMeta:   createTypeMeta("RoleBinding", "rbac.authorization.k8s.io/v1"),
		ObjectMeta: createObjectMeta(name),
		Subjects: []v1rbac.Subject{
			v1rbac.Subject{
				Kind: sa.Kind,
				Name: sa.Name,
			},
		},
		RoleRef: v1rbac.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     roleKind,
			Name:     roleName,
		},
	}
}

// createRole creates a Role given a roleName and policyRules
func createRole(roleName string, policyRules []v1rbac.PolicyRule) v1rbac.Role {
	return v1rbac.Role{
		TypeMeta:   createTypeMeta("Role", "rbac.authorization.k8s.io/v1"),
		ObjectMeta: createObjectMeta(roleName),
		Rules:      policyRules,
	}
}
